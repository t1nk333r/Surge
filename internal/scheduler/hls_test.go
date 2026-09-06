package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SurgeDM/Surge/internal/hls"
	"github.com/SurgeDM/Surge/internal/mux"
	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/types"
)

// hlsServer serves a master playlist, one media playlist and its fragments,
// counting fragment requests so a resumed download can be shown to skip work.
type hlsServer struct {
	*httptest.Server
	fragmentHits atomic.Int64
}

func newHLSServer(t *testing.T, fragments [][]byte) *hlsServer {
	t.Helper()
	srv := &hlsServer{}
	handler := http.NewServeMux()

	handler.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS=\"avc1.42001e,mp4a.40.2\",RESOLUTION=640x360\n"+
			"low/media.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=2400000,CODECS=\"avc1.64001f,mp4a.40.2\",RESOLUTION=1280x720\n"+
			"high/media.m3u8\n")
	})

	media := func(prefix string) string {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-VERSION:3\n")
		for i := range fragments {
			fmt.Fprintf(&b, "#EXTINF:4.0,\n%s/frag%d.ts\n", prefix, i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		return b.String()
	}
	handler.HandleFunc("/high/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, media("."))
	})
	handler.HandleFunc("/low/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, media("."))
	})

	for i, body := range fragments {
		content := body
		for _, quality := range []string{"high", "low"} {
			handler.HandleFunc(fmt.Sprintf("/%s/frag%d.ts", quality, i), func(w http.ResponseWriter, r *http.Request) {
				srv.fragmentHits.Add(1)
				_, _ = w.Write(content)
			})
		}
	}

	srv.Server = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func manifestRecord(t *testing.T, dir, name, manifestURL string) *types.DownloadRecord {
	t.Helper()
	state := progress.New("hls-id", 0)
	state.SetDestPath(filepath.Join(dir, name))
	return &types.DownloadRecord{
		ID:            "hls-id",
		URL:           "https://site.example/watch?v=1",
		SourceURL:     "https://site.example/watch?v=1",
		OutputPath:    dir,
		Filename:      name,
		DestPath:      filepath.Join(dir, name),
		ManifestURL:   manifestURL,
		ProgressState: state,
		Runtime:       types.DefaultRuntimeConfig(),
	}
}

func TestRunManifestDownloadAssemblesEveryFragmentInOrder(t *testing.T) {
	fragments := [][]byte{
		[]byte(strings.Repeat("a", 4096)),
		[]byte(strings.Repeat("b", 4096)),
		[]byte(strings.Repeat("c", 2048)),
	}
	server := newHLSServer(t, fragments)

	dir := t.TempDir()
	muxer := &recordingMuxer{available: true}
	withMuxer(t, muxer)

	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/master.m3u8")
	progState := progress.CfgProgress(cfg)

	size, err := runManifestDownload(context.Background(), cfg, progState, cfg.DestPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The assembled bytes must be the fragments concatenated in playlist
	// order: out-of-order fragments produce an unplayable file, and the
	// recording muxer copies its input verbatim so order is observable.
	want := strings.Repeat("a", 4096) + strings.Repeat("b", 4096) + strings.Repeat("c", 2048)
	got, err := os.ReadFile(cfg.DestPath + types.IncompleteSuffix)
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if string(got) != want {
		t.Errorf("assembled %d bytes, want the three fragments in order (%d)", len(got), len(want))
	}
	if size != int64(len(want)) {
		t.Errorf("size = %d, want %d", size, len(want))
	}
	if muxer.remuxes != 1 {
		t.Errorf("remuxes = %d, want exactly one", muxer.remuxes)
	}

	// Fragments are scaffolding; the user's directory must not keep them.
	if _, err := os.Stat(cfg.DestPath + ".frags"); !os.IsNotExist(err) {
		t.Errorf("fragment directory survived: %v", err)
	}
	if _, err := os.Stat(cfg.DestPath + ".hlsraw"); !os.IsNotExist(err) {
		t.Error("intermediate concatenation survived")
	}
}

// The highest-quality variant is the one a user asking for "the video" means.
func TestRunManifestDownloadFollowsTheBestVariant(t *testing.T) {
	server := newHLSServer(t, [][]byte{[]byte("x")})
	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})

	var requested []string
	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/master.m3u8")
	client := server.Client()
	client.Transport = recordingTransport{inner: http.DefaultTransport, seen: &requested}

	playlist, bandwidth, err := resolveMediaPlaylist(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bandwidth != 2400000 {
		t.Errorf("bandwidth = %d, want the 720p variant's", bandwidth)
	}
	if len(playlist.Segments) != 1 {
		t.Fatalf("segments = %d", len(playlist.Segments))
	}
	if !strings.Contains(playlist.Segments[0].URL, "/high/") {
		t.Errorf("segment %q does not come from the best variant", playlist.Segments[0].URL)
	}
}

// Fragments already on disk are not fetched again: that is the whole resume
// story for a fragmented stream.
func TestRunManifestDownloadSkipsFragmentsAlreadyOnDisk(t *testing.T) {
	fragments := [][]byte{[]byte("one"), []byte("two")}
	server := newHLSServer(t, fragments)

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})
	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/master.m3u8")

	fragDir := types.FragmentDirPath(cfg.DestPath)
	// A resumed download's directory carries the marker written by its first
	// attempt; without it the fragments cannot be attributed to this stream.
	if err := prepareFragmentDir(fragDir, fragmentIdentity(cfg, []hls.Segment{{}, {}})); err != nil {
		t.Fatalf("prepare fragment dir: %v", err)
	}
	if err := os.WriteFile(fragmentPath(fragDir, 0), fragments[0], 0o644); err != nil {
		t.Fatalf("seed fragment: %v", err)
	}

	if _, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits := server.fragmentHits.Load(); hits != 1 {
		t.Errorf("fetched %d fragments, want only the missing one", hits)
	}
}

// The fragment directory is named after the destination file, which the user
// frees by removing a failed download. Fragments are trusted because they
// exist, so a directory left behind by another stream must be discarded, or a
// new download of a recycled name assembles someone else's video and reports
// success.
func TestRunManifestDownloadDiscardsAnotherStreamsFragments(t *testing.T) {
	fragments := [][]byte{[]byte("one"), []byte("two")}
	server := newHLSServer(t, fragments)

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})
	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/master.m3u8")

	fragDir := types.FragmentDirPath(cfg.DestPath)
	stale := manifestRecord(t, dir, "other.mp4", "https://elsewhere.example/other.m3u8")
	if err := prepareFragmentDir(fragDir, fragmentIdentity(stale, []hls.Segment{{}, {}})); err != nil {
		t.Fatalf("prepare fragment dir: %v", err)
	}
	if err := os.WriteFile(fragmentPath(fragDir, 0), []byte("someone else's video"), 0o644); err != nil {
		t.Fatalf("seed stale fragment: %v", err)
	}

	if _, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(cfg.DestPath + types.IncompleteSuffix)
	if err != nil {
		t.Fatalf("read assembled output: %v", err)
	}
	if want := string(fragments[0]) + string(fragments[1]); string(got) != want {
		t.Errorf("assembled %q, want %q - stale fragments were reused", got, want)
	}
}

// A live stream never ends, so it has no size and no completion: refusing is
// the only honest answer.
func TestRunManifestDownloadRefusesLiveStreams(t *testing.T) {
	handler := http.NewServeMux()
	handler.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n")
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})
	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/live.m3u8")

	_, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath)
	if !errors.Is(err, hls.ErrLive) {
		t.Fatalf("err = %v, want ErrLive", err)
	}
}

// A variant whose audio is a separate rendition would download as a silent
// video, so it is refused instead.
func TestRunManifestDownloadRefusesSeparateAudioRenditions(t *testing.T) {
	handler := http.NewServeMux()
	handler.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"English\",DEFAULT=YES,URI=\"audio/media.m3u8\"\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=2400000,CODECS=\"avc1.64001f\",RESOLUTION=1280x720,AUDIO=\"aud\"\n"+
			"video/media.m3u8\n")
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true})
	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/master.m3u8")

	_, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath)
	if !errors.Is(err, ErrSeparateAudioRendition) {
		t.Fatalf("err = %v, want ErrSeparateAudioRendition", err)
	}
}

func TestRunManifestDownloadRefusesWithoutMuxer(t *testing.T) {
	withMuxer(t, &recordingMuxer{available: false})
	dir := t.TempDir()
	cfg := manifestRecord(t, dir, "stream.mp4", "https://cdn/master.m3u8")

	_, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath)
	if !errors.Is(err, mux.ErrNotAvailable) {
		t.Fatalf("err = %v, want ErrNotAvailable", err)
	}
}

// A failed assembly keeps the fragments so a retry costs nothing.
func TestRunManifestDownloadKeepsFragmentsWhenRemuxFails(t *testing.T) {
	server := newHLSServer(t, [][]byte{[]byte("one"), []byte("two")})
	dir := t.TempDir()
	withMuxer(t, &recordingMuxer{available: true, err: errors.New("ffmpeg exploded")})

	cfg := manifestRecord(t, dir, "stream.mp4", server.URL+"/master.m3u8")
	if _, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err == nil {
		t.Fatal("expected the remux failure to surface")
	}

	for i := range 2 {
		if _, err := os.Stat(fragmentPath(cfg.DestPath+".frags", i)); err != nil {
			t.Errorf("fragment %d was discarded after a failed remux: %v", i, err)
		}
	}
}

// End to end with real ffmpeg: fragments generated as MPEG-TS come down over
// HTTP and are assembled into a playable file with both streams intact.
func TestRunManifestDownloadWithFFmpeg(t *testing.T) {
	ffmpeg := mux.NewFFmpeg()
	if !ffmpeg.Available() {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	segmentDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(segmentDir, 0o755); err != nil {
		t.Fatalf("create source dir: %v", err)
	}
	// Two 1-second TS segments with video and audio, which is what an HLS
	// variant's segments are.
	cmd := exec.Command(ffmpeg.Binary, "-nostdin", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "mpeg2video", "-c:a", "mp2",
		"-f", "segment", "-segment_time", "1", "-segment_format", "mpegts",
		filepath.Join(segmentDir, "frag%d.ts"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not generate test fragments: %v: %s", err, out)
	}

	var fragments [][]byte
	for i := range 2 {
		body, err := os.ReadFile(filepath.Join(segmentDir, fmt.Sprintf("frag%d.ts", i)))
		if err != nil {
			t.Skipf("expected fragment %d: %v", i, err)
		}
		fragments = append(fragments, body)
	}

	server := newHLSServer(t, fragments)
	withMuxer(t, ffmpeg)

	cfg := manifestRecord(t, dir, "stream.mkv", server.URL+"/master.m3u8")
	if _, err := runManifestDownload(context.Background(), cfg, progress.CfgProgress(cfg), cfg.DestPath); err != nil {
		t.Fatalf("manifest download failed: %v", err)
	}

	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", cfg.DestPath+types.IncompleteSuffix).Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v", err)
	}
	// ffprobe's csv writer separates fields with commas and rows with
	// newlines; both are just separators here.
	streams := strings.FieldsFunc(string(out), func(r rune) bool {
		return r == ',' || r == '\n' || r == ' '
	})
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
		t.Errorf("assembled streams = %v, want video and audio", streams)
	}
}

// recordingTransport notes every URL requested so variant selection can be
// asserted from the outside.
type recordingTransport struct {
	inner http.RoundTripper
	seen  *[]string
}

func (r recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.seen = append(*r.seen, req.URL.String())
	return r.inner.RoundTrip(req)
}

// A ranged fragment answered with the whole resource must be refused. CDN
// edges and proxies do ignore Range; storing that body would put one full copy
// of the resource into the stream per byte-range fragment, and the rename
// would make it permanent.
func TestFetchFragmentRefusesIgnoredByteRange(t *testing.T) {
	body := []byte(strings.Repeat("x", 4096))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore r.Header.Get("Range").
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	cfg := &types.DownloadRecord{Runtime: types.DefaultRuntimeConfig()}
	fragment := hls.Segment{URL: server.URL + "/chunk", Offset: 0, Length: 512}
	path := fragmentPath(dir, 0)

	if _, err := fetchFragment(context.Background(), server.Client(), cfg, fragment, path); err == nil {
		t.Fatal("expected the ignored byte range to fail the fragment")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("fragment was stored anyway: %v", err)
	}
}

// A playlist is remote content and names its own fragment hosts, so the
// credentials the extraction was issued for must not follow a fragment to
// another host.
func TestApplyFragmentHeadersWithholdsCredentialsCrossHost(t *testing.T) {
	cfg := &types.DownloadRecord{
		SourceURL: "https://site.example/watch",
		Headers: map[string]string{
			"Cookie":        "session=secret",
			"Authorization": "Bearer secret",
			"Referer":       "https://site.example/watch",
		},
		Runtime: types.DefaultRuntimeConfig(),
	}

	same, _ := http.NewRequest(http.MethodGet, "https://site.example/frag0.ts", nil)
	applyFragmentHeaders(same, cfg, credentialHost(cfg))
	if same.Header.Get("Cookie") != "session=secret" {
		t.Errorf("same-host request lost its cookie: %q", same.Header.Get("Cookie"))
	}

	other, _ := http.NewRequest(http.MethodGet, "https://attacker.example/frag0.ts", nil)
	applyFragmentHeaders(other, cfg, credentialHost(cfg))
	if got := other.Header.Get("Cookie"); got != "" {
		t.Errorf("cookie leaked to another host: %q", got)
	}
	if got := other.Header.Get("Authorization"); got != "" {
		t.Errorf("authorization leaked to another host: %q", got)
	}
	if got := other.Header.Get("Referer"); got != "https://site.example/watch" {
		t.Errorf("non-credential header was dropped: %q", got)
	}
}
