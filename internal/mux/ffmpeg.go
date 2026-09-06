package mux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// DefaultFFmpegBinary is the executable looked up on PATH. Override it with
// SURGE_FFMPEG for a non-standard install.
const DefaultFFmpegBinary = "ffmpeg"

// FFmpeg remuxes streams with ffmpeg's stream copy (`-c copy`).
//
// ffmpeg is used as a container tool only, not as a downloader: the inputs are
// files Surge itself fetched, so segmented downloads, resume and progress
// reporting all still belong to the engine.
type FFmpeg struct {
	// Binary is the executable name or absolute path.
	Binary string

	lookupOnce sync.Once
	resolved   string
}

// NewFFmpeg returns an ffmpeg muxer honouring the SURGE_FFMPEG override.
func NewFFmpeg() *FFmpeg {
	binary := strings.TrimSpace(os.Getenv("SURGE_FFMPEG"))
	if binary == "" {
		binary = DefaultFFmpegBinary
	}
	return &FFmpeg{Binary: binary}
}

func (f *FFmpeg) Name() string { return "ffmpeg" }

// path resolves the binary once; an empty result means "not installed".
func (f *FFmpeg) path() string {
	f.lookupOnce.Do(func() {
		binary := strings.TrimSpace(f.Binary)
		if binary == "" {
			binary = DefaultFFmpegBinary
		}
		if strings.ContainsRune(binary, os.PathSeparator) {
			if info, err := os.Stat(binary); err == nil && !info.IsDir() {
				f.resolved = binary
			}
			return
		}
		if found, err := exec.LookPath(binary); err == nil {
			f.resolved = found
		}
	})
	return f.resolved
}

func (f *FFmpeg) Available() bool { return f.path() != "" }

// Mux writes the combined file to destPath, replacing it if present.
func (f *FFmpeg) Mux(ctx context.Context, destPath string, inputs ...Input) error {
	// Arguments are validated before the tool is looked up: a bad call is the
	// caller's bug and must not be reported as a missing ffmpeg.
	ordered, err := prepareInputs(inputs)
	if err != nil {
		return fmt.Errorf("mux %s: %w", destPath, err)
	}

	binary := f.path()
	if binary == "" {
		return ErrNotAvailable
	}

	tmpPath, err := createTempOutput(destPath)
	if err != nil {
		return fmt.Errorf("mux %s: %w", destPath, err)
	}
	// Only a successful run renames the temp file into place; every other exit
	// removes it, so a failed or cancelled mux never leaves a truncated file
	// where a complete download is expected.
	defer os.Remove(tmpPath)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, ffmpegArgs(tmpPath, ordered)...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("mux %s aborted: %w", destPath, ctxErr)
		}
		if message := firstLine(stderr.String()); message != "" {
			return fmt.Errorf("mux %s: ffmpeg failed: %s", destPath, message)
		}
		return fmt.Errorf("mux %s: ffmpeg failed: %w", destPath, err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("mux %s: %w", destPath, err)
	}
	return nil
}

// Remux rewrites srcPath into destPath's container without touching the
// streams. Unlike Mux it maps everything (`-map 0`) rather than one video plus
// one audio: the source is already a finished multiplex — a concatenated HLS
// stream — and dropping a stream here would lose part of the download.
func (f *FFmpeg) Remux(ctx context.Context, destPath string, srcPath string) error {
	if info, err := os.Stat(srcPath); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("remux %q: %w", srcPath, ErrInputMissing)
	}

	binary := f.path()
	if binary == "" {
		return ErrNotAvailable
	}

	tmpPath, err := createTempOutput(destPath)
	if err != nil {
		return fmt.Errorf("remux %s: %w", destPath, err)
	}
	defer os.Remove(tmpPath)

	args := []string{"-nostdin", "-y", "-loglevel", "error", "-i", srcPath, "-map", "0", "-c", "copy"}
	if extFamily(filepath.Ext(destPath)) == familyMP4 {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, tmpPath)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("remux %s aborted: %w", destPath, ctxErr)
		}
		if message := firstLine(stderr.String()); message != "" {
			return fmt.Errorf("remux %s: ffmpeg failed: %s", destPath, message)
		}
		return fmt.Errorf("remux %s: ffmpeg failed: %w", destPath, err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("remux %s: %w", destPath, err)
	}
	return nil
}

// prepareInputs validates the inputs and returns them in ffmpeg input order.
func prepareInputs(inputs []Input) ([]Input, error) {
	if len(inputs) == 0 {
		return nil, ErrNoInputs
	}

	seen := make(map[Kind]bool, len(inputs))
	for _, in := range inputs {
		switch in.Kind {
		case KindVideo, KindAudio:
		default:
			return nil, fmt.Errorf("%q: %w", string(in.Kind), ErrUnknownKind)
		}
		if seen[in.Kind] {
			return nil, fmt.Errorf("%s: %w", in.Kind, ErrDuplicateKind)
		}
		seen[in.Kind] = true

		// Checked up front so a missing part is reported as such instead of as
		// an opaque ffmpeg error, and so nothing is started for a lost file.
		info, err := os.Stat(in.Path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s %q: %w", in.Kind, in.Path, ErrInputMissing)
		}
	}

	ordered := slices.Clone(inputs)
	// Video first: ffmpeg emits streams in mapping order, and players,
	// thumbnailers and `-c copy` remuxes downstream all expect stream 0 to be
	// the picture. Sorting also makes the argv deterministic for a given pair.
	slices.SortStableFunc(ordered, func(a, b Input) int {
		return kindRank(a.Kind) - kindRank(b.Kind)
	})
	return ordered, nil
}

func kindRank(kind Kind) int {
	if kind == KindVideo {
		return 0
	}
	return 1
}

// ffmpegArgs builds the argument list for writing outPath from validated,
// ordered inputs.
func ffmpegArgs(outPath string, inputs []Input) []string {
	args := make([]string, 0, 4*len(inputs)+9)

	// -nostdin so ffmpeg never steals Surge's stdin or blocks on a prompt, -y
	// so the (already reserved) output file is overwritten instead of asked
	// about, -loglevel error so only real problems reach the captured stderr.
	args = append(args, "-nostdin", "-y", "-loglevel", "error")
	for _, in := range inputs {
		args = append(args, "-i", in.Path)
	}
	for i, in := range inputs {
		// Every wanted stream is mapped by hand: ffmpeg's default mapping
		// picks the single "best" stream per type across all inputs, which
		// silently drops one of the files we just downloaded.
		args = append(args, "-map", fmt.Sprintf("%d:%s:0", i, streamSpecifier(in.Kind)))
	}

	// Container remux only; the streams are already the codecs we asked for.
	args = append(args, "-c", "copy")
	if extFamily(filepath.Ext(outPath)) == familyMP4 {
		// These files are usually watched, often while still being copied off
		// the machine, so the moov atom belongs at the front of the file.
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, outPath)
}

func streamSpecifier(kind Kind) string {
	if kind == KindVideo {
		return "v"
	}
	return "a"
}

// createTempOutput reserves a hidden temp file next to destPath. It keeps
// destPath's extension because ffmpeg picks the output muxer from it, and it
// lives in the same directory so the final rename is atomic.
func createTempOutput(destPath string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(destPath), ".surge-mux-*"+filepath.Ext(destPath))
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// firstLine keeps errors to ffmpeg's actual complaint; the rest of its stderr
// is banner and progress noise.
func firstLine(value string) string {
	trimmed := strings.TrimSpace(value)
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		trimmed = trimmed[:i]
	}
	return strings.TrimSpace(trimmed)
}
