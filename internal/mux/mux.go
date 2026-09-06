// Package mux combines the separate video and audio streams a site serves into
// the single playable file a user expects a download to produce.
//
// Muxing here is strictly a container operation: the downloaded streams are
// already in their final codecs, so they are stream-copied into the new
// container. Re-encoding would cost minutes of CPU per download and lose
// quality for no gain, which is also why a muxer can never repair a broken
// stream — it either remuxes byte-for-byte or fails. Failures leave the source
// files untouched so the caller can retry the mux without downloading again.
package mux

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrNotAvailable means the backing tool is not installed.
	ErrNotAvailable = errors.New("muxer not available")

	// ErrNoInputs means Mux was called with nothing to combine.
	ErrNoInputs = errors.New("no inputs to mux")

	// ErrDuplicateKind means two inputs claim the same stream kind. Surge
	// muxes one video track with one audio track; with two of a kind the
	// choice of which one survives would be arbitrary, so it is refused.
	ErrDuplicateKind = errors.New("duplicate input kind")

	// ErrUnknownKind means an input's Kind is neither video nor audio.
	ErrUnknownKind = errors.New("unknown input kind")

	// ErrInputMissing means an input path is absent or not a regular file.
	ErrInputMissing = errors.New("input file missing")
)

// Kind is the stream an Input carries. It is a string so errors and logs name
// the offending stream without a lookup table.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Input is one source stream.
type Input struct {
	Path string // local file already downloaded
	Kind Kind   // KindVideo or KindAudio
}

// Muxer combines separate media streams into one container file.
type Muxer interface {
	// Name identifies the muxer in logs and errors.
	Name() string

	// Available reports whether the muxer can run at all (tool installed).
	Available() bool

	// Mux writes the combined file to destPath, replacing it if present.
	Mux(ctx context.Context, destPath string, inputs ...Input) error

	// Remux rewrites srcPath into destPath's container, copying every stream.
	// It is what turns a concatenated fragment stream (an HLS download) into
	// the file a player expects.
	Remux(ctx context.Context, destPath string, srcPath string) error
}

// Container extensions this package can target.
const (
	containerMP4      = "mp4"
	containerWebM     = "webm"
	containerMatroska = "mkv"
)

// ContainerFor returns the container extension (no leading dot) to use when
// muxing a video stream of videoExt with an audio stream of audioExt.
//
// Matching pairs keep their native container, so an mp4 video with its m4a
// audio stays an mp4 that every device plays. A mixed pair falls back to
// Matroska: it is the only widely supported container that accepts any codec
// combination, so e.g. VP9 video with AAC audio still remuxes losslessly
// instead of forcing a re-encode into a container that would reject it. The
// same applies to unknown extensions, where guessing wrong is the worse bet.
func ContainerFor(videoExt, audioExt string) string {
	video, audio := extFamily(videoExt), extFamily(audioExt)
	if video == audio {
		switch video {
		case familyMP4:
			return containerMP4
		case familyWebM:
			return containerWebM
		}
	}
	return containerMatroska
}

// family groups extensions that share one container format.
type family int

const (
	familyOther family = iota
	familyMP4
	familyWebM
)

func extFamily(ext string) family {
	switch normalizeExt(ext) {
	case "mp4", "m4v", "m4a", "m4b", "aac":
		return familyMP4
	case "webm", "weba", "opus":
		return familyWebM
	}
	return familyOther
}

// normalizeExt accepts both "mp4" and ".MP4", since extensions reach us from
// site metadata and from filepath.Ext alike.
func normalizeExt(ext string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(ext), "."))
}
