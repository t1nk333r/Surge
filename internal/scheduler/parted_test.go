package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SurgeDM/Surge/internal/mux"
	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/types"
)

// recordingMuxer stands in for ffmpeg so the engine path can be tested without
// real media.
type recordingMuxer struct {
	available bool
	err       error
	calls     int
	inputs    []mux.Input
	dest      string
	remuxes   int
	remuxSrc  string
}

func (m *recordingMuxer) Name() string    { return "recording" }
func (m *recordingMuxer) Available() bool { return m.available }

func (m *recordingMuxer) Mux(_ context.Context, destPath string, inputs ...mux.Input) error {
	m.calls++
	m.dest = destPath
	m.inputs = append([]mux.Input(nil), inputs...)
	if m.err != nil {
		return m.err
	}
	// Stand in for a container: the engine only cares that the file exists and
	// has a size.
	var combined []byte
	for _, in := range inputs {
		data, err := os.ReadFile(in.Path)
		if err != nil {
			return err
		}
		combined = append(combined, data...)
	}
	return os.WriteFile(destPath, combined, 0o644)
}

// Remux stands in for the container rewrite an assembled fragment stream
// needs; it copies the source so callers can assert the assembled bytes.
func (m *recordingMuxer) Remux(_ context.Context, destPath string, srcPath string) error {
	m.remuxes++
	m.remuxSrc = srcPath
	m.dest = destPath
	if m.err != nil {
		return m.err
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, data, 0o644)
}

func withMuxer(t *testing.T, m mux.Muxer) {
	t.Helper()
	previous := defaultMuxer
	defaultMuxer = m
	t.Cleanup(func() { defaultMuxer = previous })
}

// streamServer serves fixed bodies with range support, which is what the
// concurrent downloader expects of a CDN.
func streamServer(t *testing.T, bodies map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, body := range bodies {
		content := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, filepath.Base(r.URL.Path), testModTime, strings.NewReader(string(content)))
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func partedRecord(t *testing.T, dir, name string, videoURL, audioURL string, videoSize, audioSize int64) *types.DownloadRecord {
	t.Helper()
	state := progress.New("parted-id", videoSize+audioSize)
	state.SetDestPath(filepath.Join(dir, name))
	return &types.DownloadRecord{
		ID:            "parted-id",
		URL:           "https://site.example/watch?v=1",
		OutputPath:    dir,
		Filename:      name,
		DestPath:      filepath.Join(dir, name),
		TotalSize:     videoSize + audioSize,
		SourceURL:     "https://site.example/watch?v=1",
		ProgressState: state,
		Runtime:       types.DefaultRuntimeConfig(),
		Parts: []types.DownloadPart{
			{URL: videoURL, Kind: types.PartKindVideo, Size: videoSize, FormatID: "137"},
			{URL: audioURL, Kind: types.PartKindAudio, Size: audioSize, FormatID: "251"},
		},
	}
}

func TestRunPartedDownloadFetchesBothStreamsAndMuxes(t *testing.T) {
	video := []byte(strings.Repeat("v", 300000))
	audio := []byte(strings.Repeat("a", 100000))
	server := streamServer(t, map[string][]byte{"/video": video, "/audio": audio})

	dir := t.TempDir()
	muxer := &recordingMuxer{available: true}
	withMuxer(t, muxer)

	cfg := partedRecord(t, dir, "clip.mkv", server.URL+"/video", server.URL+"/audio", int64(len(video)), int64(len(audio)))
	progState := progress.CfgProgress(cfg)

	size, err := runPartedDownload(context.Background(), cfg, progState, cfg.DestPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if muxer.calls != 1 {
		t.Fatalf("muxer called %d times", muxer.calls)
	}
	// ffmpeg needs a container extension, so the mux writes a sibling and the
	// engine moves that onto the reserved working file.
	if muxer.dest != cfg.DestPath+".muxing"+filepath.Ext(cfg.DestPath) {
		t.Errorf("mux dest = %q", muxer.dest)
	}
	if _, err := os.Stat(muxer.dest); !os.IsNotExist(err) {
		t.Errorf("intermediate mux file %s was left behind", muxer.dest)
	}
	if len(muxer.inputs) != 2 || muxer.inputs[0].Kind != mux.KindVideo || muxer.inputs[1].Kind != mux.KindAudio {
		t.Errorf("mux inputs = %+v", muxer.inputs)
	}

	expected := int64(len(video) + len(audio))
	if size != expected {
		t.Errorf("size = %d, want the muxed file size %d", size, expected)
	}
	out, err := os.ReadFile(cfg.DestPath + types.IncompleteSuffix)
	if err != nil {
		t.Fatalf("read muxed output: %v", err)
	}
	if int64(len(out)) != expected {
		t.Errorf("muxed output is %d bytes, want %d", len(out), expected)
	}

	// Part working files are temporary scaffolding and must not be left next
	// to the user's file.
	for i, part := range cfg.Parts {
		leftover := types.PartWorkingPath(cfg.DestPath, i, part.Kind) + types.IncompleteSuffix
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("part file %s survived the mux", leftover)
		}
	}

	// Combined progress must end at the whole item, not at one part.
	downloaded, total, _, _, _, _ := progState.GetProgress()
	if downloaded != expected || total != expected {
		t.Errorf("progress = %d/%d, want %d/%d", downloaded, total, expected, expected)
	}
}

// A stream recorded as finished must not be fetched again: that is what makes
// a resumed multi-part download cheap.
func TestRunPartedDownloadSkipsCompleteParts(t *testing.T) {
	video := []byte(strings.Repeat("v", 50000))
	audio := []byte(strings.Repeat("a", 20000))

	var videoRequests int
	handler := http.NewServeMux()
	handler.HandleFunc("/video", func(w http.ResponseWriter, r *http.Request) {
		videoRequests++
		http.ServeContent(w, r, "video", testModTime, strings.NewReader(string(video)))
	})
	handler.HandleFunc("/audio", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "audio", testModTime, strings.NewReader(string(audio)))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})

	cfg := partedRecord(t, dir, "clip.mkv", server.URL+"/video", server.URL+"/audio", int64(len(video)), int64(len(audio)))
	cfg.Parts[0].Complete = true

	videoWorking := types.PartWorkingPath(cfg.DestPath, 0, types.PartKindVideo) + types.IncompleteSuffix
	if err := os.WriteFile(videoWorking, video, 0o644); err != nil {
		t.Fatalf("seed part file: %v", err)
	}

	if _, err := runPartedDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if videoRequests != 0 {
		t.Errorf("video was re-fetched %d times despite being recorded complete", videoRequests)
	}
}

// A recorded stream whose working file is gone must be downloaded again. The
// file can vanish (the store's integrity sweep, a failed move, the user), and
// muxing the zero-byte file the downloader would otherwise recreate yields a
// download that fails identically on every retry.
func TestRunPartedDownloadRefetchesCompletePartWithoutItsFile(t *testing.T) {
	video := []byte(strings.Repeat("v", 30000))
	audio := []byte(strings.Repeat("a", 12000))
	server := streamServer(t, map[string][]byte{"/video": video, "/audio": audio})

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})

	cfg := partedRecord(t, dir, "clip.mkv", server.URL+"/video", server.URL+"/audio", int64(len(video)), int64(len(audio)))
	cfg.Parts[0].Complete = true // recorded complete, but nothing on disk

	if _, err := runPartedDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(cfg.DestPath + types.IncompleteSuffix)
	if err != nil {
		t.Fatalf("read muxed output: %v", err)
	}
	if string(got) != string(video)+string(audio) {
		t.Errorf("muxed output is %d bytes, want the %d of both streams", len(got), len(video)+len(audio))
	}
}

// Completion cannot be inferred from the working file's size: the concurrent
// downloader preallocates it to the full length, so a barely-started part
// looks finished on disk. Trusting that muxed a 26 MB stub out of a 1.3 GB
// video on the live service, which is why this is pinned.
func TestRunPartedDownloadDistrustsPreallocatedPartFiles(t *testing.T) {
	video := []byte(strings.Repeat("v", 40000))
	audio := []byte(strings.Repeat("a", 10000))
	server := streamServer(t, map[string][]byte{"/video": video, "/audio": audio})

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})

	cfg := partedRecord(t, dir, "clip.mkv", server.URL+"/video", server.URL+"/audio", int64(len(video)), int64(len(audio)))

	// A preallocated, almost empty working file: right size, no content, and
	// no completion recorded.
	videoWorking := types.PartWorkingPath(cfg.DestPath, 0, types.PartKindVideo) + types.IncompleteSuffix
	if err := os.WriteFile(videoWorking, make([]byte, len(video)), 0o644); err != nil {
		t.Fatalf("seed preallocated part: %v", err)
	}

	if _, err := runPartedDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parts are consumed by the mux, so the proof is in the output: the
	// recording muxer concatenates its inputs verbatim.
	got, err := os.ReadFile(cfg.DestPath + types.IncompleteSuffix)
	if err != nil {
		t.Fatalf("read muxed output: %v", err)
	}
	if string(got) != string(video)+string(audio) {
		t.Error("the preallocated part was treated as complete instead of being downloaded")
	}
}

func TestRunPartedDownloadRefusesWithoutMuxer(t *testing.T) {
	withMuxer(t, &recordingMuxer{available: false})

	dir := t.TempDir()
	cfg := partedRecord(t, dir, "clip.mkv", "https://cdn/video", "https://cdn/audio", 10, 10)

	_, err := runPartedDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath)
	if !errors.Is(err, mux.ErrNotAvailable) {
		t.Fatalf("err = %v, want ErrNotAvailable", err)
	}
}

// A failed mux must not leave the download looking finished, and must keep the
// downloaded parts so a retry does not start from zero.
func TestRunPartedDownloadKeepsPartsWhenMuxFails(t *testing.T) {
	video := []byte(strings.Repeat("v", 20000))
	audio := []byte(strings.Repeat("a", 10000))
	server := streamServer(t, map[string][]byte{"/video": video, "/audio": audio})

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true, err: errors.New("ffmpeg exploded")})

	cfg := partedRecord(t, dir, "clip.mkv", server.URL+"/video", server.URL+"/audio", int64(len(video)), int64(len(audio)))

	_, err := runPartedDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath)
	if err == nil {
		t.Fatal("expected the mux failure to surface")
	}
	for i, part := range cfg.Parts {
		partFile := types.PartWorkingPath(cfg.DestPath, i, part.Kind) + types.IncompleteSuffix
		if _, statErr := os.Stat(partFile); statErr != nil {
			t.Errorf("part %d was deleted after a failed mux: %v", i, statErr)
		}
	}
}

// End to end with the real muxer: two generated media streams come down over
// HTTP and ffmpeg joins them into a playable file.
func TestRunPartedDownloadWithFFmpeg(t *testing.T) {
	ffmpeg := mux.NewFFmpeg()
	if !ffmpeg.Available() {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	videoPath := filepath.Join(dir, "src-video.mp4")
	audioPath := filepath.Join(dir, "src-audio.m4a")
	generate := func(args ...string) {
		t.Helper()
		cmd := exec.Command(ffmpeg.Binary, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("could not generate test media: %v: %s", err, out)
		}
	}
	generate("-nostdin", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=1", "-c:v", "mpeg4", videoPath)
	generate("-nostdin", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac", audioPath)

	videoBody, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatalf("read generated video: %v", err)
	}
	audioBody, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatalf("read generated audio: %v", err)
	}

	server := streamServer(t, map[string][]byte{"/video.mp4": videoBody, "/audio.m4a": audioBody})
	withMuxer(t, ffmpeg)

	cfg := partedRecord(t, dir, "clip.mp4", server.URL+"/video.mp4", server.URL+"/audio.m4a", int64(len(videoBody)), int64(len(audioBody)))

	if _, err := runPartedDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("parted download failed: %v", err)
	}

	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", cfg.DestPath+types.IncompleteSuffix).Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v", err)
	}
	streams := strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " "))
	if len(streams) != 2 {
		t.Fatalf("muxed file has streams %v, want one video and one audio", streams)
	}
	var haveVideo, haveAudio bool
	for _, s := range streams {
		switch s {
		case "video":
			haveVideo = true
		case "audio":
			haveAudio = true
		}
	}
	if !haveVideo || !haveAudio {
		t.Errorf("muxed file streams = %v", streams)
	}
}

// testModTime keeps http.ServeContent deterministic.
var testModTime = time.Unix(1700000000, 0)
