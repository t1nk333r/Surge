package mux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestContainerFor(t *testing.T) {
	tests := []struct {
		name     string
		videoExt string
		audioExt string
		want     string
	}{
		{name: "mp4 with m4a stays mp4", videoExt: "mp4", audioExt: "m4a", want: "mp4"},
		{name: "mp4 with aac stays mp4", videoExt: "mp4", audioExt: "aac", want: "mp4"},
		{name: "m4v with m4a stays mp4", videoExt: "m4v", audioExt: "m4b", want: "mp4"},
		{name: "webm with webm stays webm", videoExt: "webm", audioExt: "webm", want: "webm"},
		{name: "webm with opus stays webm", videoExt: "webm", audioExt: "opus", want: "webm"},
		{name: "leading dots and case ignored", videoExt: ".WEBM", audioExt: " .Opus ", want: "webm"},
		{name: "webm video with m4a audio needs mkv", videoExt: "webm", audioExt: "m4a", want: "mkv"},
		{name: "mp4 video with opus audio needs mkv", videoExt: "mp4", audioExt: "opus", want: "mkv"},
		{name: "unknown video extension needs mkv", videoExt: "flv", audioExt: "m4a", want: "mkv"},
		{name: "unknown audio extension needs mkv", videoExt: "mp4", audioExt: "mp3", want: "mkv"},
		{name: "both unknown needs mkv", videoExt: "", audioExt: "", want: "mkv"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainerFor(tc.videoExt, tc.audioExt); got != tc.want {
				t.Fatalf("ContainerFor(%q, %q) = %q, want %q", tc.videoExt, tc.audioExt, got, tc.want)
			}
		})
	}
}

// The muxer is addressed through the interface it promises callers.
var _ Muxer = (*FFmpeg)(nil)

func TestMuxRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	video := writeFile(t, filepath.Join(dir, "video.mp4"), "video bytes")
	audio := writeFile(t, filepath.Join(dir, "audio.m4a"), "audio bytes")

	tests := []struct {
		name   string
		inputs []Input
		want   error
	}{
		{
			name:   "no inputs",
			inputs: nil,
			want:   ErrNoInputs,
		},
		{
			name:   "two video inputs",
			inputs: []Input{{Path: video, Kind: KindVideo}, {Path: video, Kind: KindVideo}},
			want:   ErrDuplicateKind,
		},
		{
			name:   "two audio inputs",
			inputs: []Input{{Path: audio, Kind: KindAudio}, {Path: audio, Kind: KindAudio}},
			want:   ErrDuplicateKind,
		},
		{
			name:   "unknown kind",
			inputs: []Input{{Path: video, Kind: Kind("subtitle")}},
			want:   ErrUnknownKind,
		},
		{
			name:   "missing input file",
			inputs: []Input{{Path: video, Kind: KindVideo}, {Path: filepath.Join(dir, "gone.m4a"), Kind: KindAudio}},
			want:   ErrInputMissing,
		},
		{
			name:   "empty input path",
			inputs: []Input{{Path: "", Kind: KindAudio}},
			want:   ErrInputMissing,
		},
		{
			name:   "directory as input",
			inputs: []Input{{Path: dir, Kind: KindVideo}},
			want:   ErrInputMissing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A binary that cannot exist: argument errors must be reported as
			// themselves, not masked by a missing ffmpeg.
			m := &FFmpeg{Binary: filepath.Join(dir, "no-such-ffmpeg")}
			dest := filepath.Join(dir, "out.mp4")

			err := m.Mux(context.Background(), dest, tc.inputs...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Mux error = %v, want %v", err, tc.want)
			}
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Fatalf("destination exists after refused mux: %v", statErr)
			}
			assertNoTempLeftovers(t, dir)
		})
	}
}

func TestMuxWithoutBinaryIsNotAvailable(t *testing.T) {
	dir := t.TempDir()
	video := writeFile(t, filepath.Join(dir, "video.mp4"), "video bytes")
	audio := writeFile(t, filepath.Join(dir, "audio.m4a"), "audio bytes")

	m := &FFmpeg{Binary: filepath.Join(dir, "no-such-ffmpeg")}
	if m.Available() {
		t.Fatal("Available() = true for a nonexistent binary")
	}

	err := m.Mux(context.Background(), filepath.Join(dir, "out.mp4"),
		Input{Path: video, Kind: KindVideo}, Input{Path: audio, Kind: KindAudio})
	if !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("Mux error = %v, want %v", err, ErrNotAvailable)
	}
}

func TestNewFFmpegHonoursEnvOverride(t *testing.T) {
	t.Setenv("SURGE_FFMPEG", "/opt/ffmpeg/bin/ffmpeg")
	if got := NewFFmpeg().Binary; got != "/opt/ffmpeg/bin/ffmpeg" {
		t.Fatalf("Binary = %q, want the SURGE_FFMPEG value", got)
	}

	t.Setenv("SURGE_FFMPEG", "  ")
	if got := NewFFmpeg().Binary; got != DefaultFFmpegBinary {
		t.Fatalf("Binary = %q, want %q", got, DefaultFFmpegBinary)
	}
}

func TestFFmpegArgs(t *testing.T) {
	tests := []struct {
		name    string
		outPath string
		// Inputs are given in the order Mux resolves them to; ordering itself
		// is covered by TestPrepareInputsOrdersVideoFirst.
		inputs []Input
		want   []string
	}{
		{
			name:    "mp4 pair gets faststart",
			outPath: "/downloads/clip.mp4",
			inputs:  []Input{{Path: "/downloads/clip.mp4", Kind: KindVideo}, {Path: "/downloads/clip.m4a", Kind: KindAudio}},
			want: []string{
				"-nostdin", "-y", "-loglevel", "error",
				"-i", "/downloads/clip.mp4",
				"-i", "/downloads/clip.m4a",
				"-map", "0:v:0",
				"-map", "1:a:0",
				"-c", "copy",
				"-movflags", "+faststart",
				"/downloads/clip.mp4",
			},
		},
		{
			name:    "webm pair has no faststart",
			outPath: "/downloads/clip.webm",
			inputs:  []Input{{Path: "/downloads/video.webm", Kind: KindVideo}, {Path: "/downloads/audio.webm", Kind: KindAudio}},
			want: []string{
				"-nostdin", "-y", "-loglevel", "error",
				"-i", "/downloads/video.webm",
				"-i", "/downloads/audio.webm",
				"-map", "0:v:0",
				"-map", "1:a:0",
				"-c", "copy",
				"/downloads/clip.webm",
			},
		},
		{
			name:    "mixed pair into mkv has no faststart",
			outPath: "/downloads/clip.mkv",
			inputs:  []Input{{Path: "/downloads/video.webm", Kind: KindVideo}, {Path: "/downloads/audio.m4a", Kind: KindAudio}},
			want: []string{
				"-nostdin", "-y", "-loglevel", "error",
				"-i", "/downloads/video.webm",
				"-i", "/downloads/audio.m4a",
				"-map", "0:v:0",
				"-map", "1:a:0",
				"-c", "copy",
				"/downloads/clip.mkv",
			},
		},
		{
			name:    "audio only input maps only audio",
			outPath: "/downloads/clip.m4a",
			inputs:  []Input{{Path: "/downloads/audio.m4a", Kind: KindAudio}},
			want: []string{
				"-nostdin", "-y", "-loglevel", "error",
				"-i", "/downloads/audio.m4a",
				"-map", "0:a:0",
				"-c", "copy",
				"-movflags", "+faststart",
				"/downloads/clip.m4a",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ffmpegArgs(tc.outPath, tc.inputs)
			t.Logf("ffmpeg %s", strings.Join(got, " "))
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("argv =\n%v\nwant\n%v", got, tc.want)
			}
		})
	}
}

// The caller may hand over the streams in any order; ffmpeg emits streams in
// mapping order, so video has to end up as input 0 either way.
func TestPrepareInputsOrdersVideoFirst(t *testing.T) {
	dir := t.TempDir()
	video := writeFile(t, filepath.Join(dir, "video.mp4"), "video bytes")
	audio := writeFile(t, filepath.Join(dir, "audio.m4a"), "audio bytes")

	ordered, err := prepareInputs([]Input{{Path: audio, Kind: KindAudio}, {Path: video, Kind: KindVideo}})
	if err != nil {
		t.Fatalf("prepareInputs: %v", err)
	}
	if len(ordered) != 2 || ordered[0].Path != video || ordered[1].Path != audio {
		t.Fatalf("ordered = %v, want video first", ordered)
	}
}

func TestFFmpegMuxEndToEnd(t *testing.T) {
	m := NewFFmpeg()
	if !m.Available() {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	tests := []struct {
		name      string
		videoName string
		videoArgs []string
		audioName string
		audioArgs []string
		wantExt   string
	}{
		{
			name:      "mp4 video with m4a audio",
			videoName: "video.mp4",
			videoArgs: []string{"-c:v", "mpeg4", "-pix_fmt", "yuv420p"},
			audioName: "audio.m4a",
			audioArgs: []string{"-c:a", "aac"},
			wantExt:   "mp4",
		},
		{
			name:      "webm video with webm audio",
			videoName: "video.webm",
			videoArgs: []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8"},
			audioName: "audio.webm",
			audioArgs: []string{"-c:a", "libopus"},
			wantExt:   "webm",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			video := filepath.Join(dir, tc.videoName)
			audio := filepath.Join(dir, tc.audioName)

			// lavfi sources keep the test self-contained: no fixture files and
			// no network, but real streams in real containers.
			if err := runFFmpeg(m, append([]string{
				"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=15",
			}, append(tc.videoArgs, video)...)); err != nil {
				t.Skipf("cannot generate %s here: %v", tc.videoName, err)
			}
			if err := runFFmpeg(m, append([]string{
				"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			}, append(tc.audioArgs, audio)...)); err != nil {
				t.Skipf("cannot generate %s here: %v", tc.audioName, err)
			}

			gotExt := ContainerFor(filepath.Ext(video), filepath.Ext(audio))
			if gotExt != tc.wantExt {
				t.Fatalf("ContainerFor = %q, want %q", gotExt, tc.wantExt)
			}
			dest := filepath.Join(dir, "muxed."+gotExt)

			if err := m.Mux(context.Background(), dest,
				Input{Path: audio, Kind: KindAudio},
				Input{Path: video, Kind: KindVideo},
			); err != nil {
				t.Fatalf("Mux: %v", err)
			}

			streams, duration := probe(t, ffprobe, dest)
			t.Logf("%s: streams=%v duration=%.3fs", filepath.Base(dest), streams, duration)

			var videoCount, audioCount int
			for _, kind := range streams {
				switch kind {
				case "video":
					videoCount++
				case "audio":
					audioCount++
				}
			}
			if videoCount != 1 || audioCount != 1 || len(streams) != 2 {
				t.Fatalf("streams = %v, want exactly one video and one audio", streams)
			}
			if duration < 0.5 || duration > 2 {
				t.Fatalf("duration = %.3fs, want roughly 1s", duration)
			}
			assertNoTempLeftovers(t, dir)

			// Replacing an existing destination is part of the contract.
			if err := m.Mux(context.Background(), dest,
				Input{Path: video, Kind: KindVideo},
				Input{Path: audio, Kind: KindAudio},
			); err != nil {
				t.Fatalf("Mux over existing destination: %v", err)
			}
			assertNoTempLeftovers(t, dir)
		})
	}
}

func TestFFmpegMuxFailureLeavesNoDestination(t *testing.T) {
	m := NewFFmpeg()
	if !m.Available() {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	video := writeFile(t, filepath.Join(dir, "video.mp4"), "not media at all")
	audio := writeFile(t, filepath.Join(dir, "audio.m4a"), "also not media")
	dest := filepath.Join(dir, "muxed.mp4")

	err := m.Mux(context.Background(), dest,
		Input{Path: video, Kind: KindVideo},
		Input{Path: audio, Kind: KindAudio},
	)
	if err == nil {
		t.Fatal("Mux of non-media inputs succeeded")
	}
	t.Logf("failure error: %v", err)
	if !strings.Contains(err.Error(), "ffmpeg failed") {
		t.Fatalf("error %q does not report the ffmpeg failure", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destination exists after failed mux: %v", statErr)
	}
	assertNoTempLeftovers(t, dir)
}

func TestFFmpegMuxHonoursCancelledContext(t *testing.T) {
	m := NewFFmpeg()
	if !m.Available() {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	video := writeFile(t, filepath.Join(dir, "video.mp4"), "not media at all")
	audio := writeFile(t, filepath.Join(dir, "audio.m4a"), "also not media")
	dest := filepath.Join(dir, "muxed.mp4")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.Mux(ctx, dest, Input{Path: video, Kind: KindVideo}, Input{Path: audio, Kind: KindAudio})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Mux error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destination exists after cancelled mux: %v", statErr)
	}
	assertNoTempLeftovers(t, dir)
}

func runFFmpeg(m *FFmpeg, args []string) error {
	cmd := exec.Command(m.path(), append([]string{"-nostdin", "-y", "-loglevel", "error"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)) + " (" + err.Error() + ")")
	}
	return nil
}

func probe(t *testing.T, ffprobe, path string) (streams []string, duration float64) {
	t.Helper()

	out, err := exec.Command(ffprobe,
		"-v", "error",
		"-show_entries", "stream=codec_type:format=duration",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}

	var report struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("decoding ffprobe output %q: %v", out, err)
	}

	for _, stream := range report.Streams {
		streams = append(streams, stream.CodecType)
	}
	duration, err = strconv.ParseFloat(report.Format.Duration, 64)
	if err != nil {
		t.Fatalf("parsing duration %q: %v", report.Format.Duration, err)
	}
	return streams, duration
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// assertNoTempLeftovers guards the temp-file discipline: no matter how a mux
// ends, the destination directory must not accumulate partial output.
func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".surge-mux-") {
			t.Fatalf("leftover temporary file %s", entry.Name())
		}
	}
}
