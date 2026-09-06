package types

import (
	"encoding/binary"
	"fmt"
)

// Task represents a byte range to download.
type Task struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

func (t *Task) GobEncode() ([]byte, error) {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:8], uint64(t.Offset))
	binary.LittleEndian.PutUint64(b[8:16], uint64(t.Length))
	return b, nil
}

func (t *Task) GobDecode(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) < 16 {
		return fmt.Errorf("corrupt task data: expected 16 bytes, got %d", len(data))
	}
	t.Offset = int64(binary.LittleEndian.Uint64(data[0:8]))
	t.Length = int64(binary.LittleEndian.Uint64(data[8:16]))
	return nil
}

// DownloadRecord is the canonical representation of a download's static configuration,
// persistent state, and runtime options. It replaces DownloadState, DownloadEntry, and DownloadConfig.
type DownloadRecord struct {
	// Identity & Core Info
	ID         string `json:"id"`
	URLHash    string `json:"url_hash"`
	URL        string `json:"url"`
	Filename   string `json:"filename"`
	OutputPath string `json:"output_path"`
	DestPath   string `json:"dest_path"`
	TotalSize  int64  `json:"total_size"`
	Downloaded int64  `json:"downloaded"`

	// Status
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	// Lifecycle Timestamps & Stats
	CreatedAt   int64   `json:"created_at"`
	PausedAt    int64   `json:"paused_at,omitempty"`
	CompletedAt int64   `json:"completed_at,omitempty"`
	TimeTaken   int64   `json:"time_taken,omitempty"`
	Elapsed     int64   `json:"elapsed,omitempty"`
	AvgSpeed    float64 `json:"avg_speed,omitempty"`

	// Resume State (Persistent)
	Tasks           []Task `json:"tasks,omitempty"`
	ChunkBitmap     []byte `json:"chunk_bitmap,omitempty"`
	ActualChunkSize int64  `json:"actual_chunk_size,omitempty"`
	FileHash        string `json:"file_hash,omitempty"`

	// Configuration Options (Persistent)
	Mirrors      []string `json:"mirrors,omitempty"`
	RateLimit    int64    `json:"rate_limit,omitempty"`
	RateLimitSet bool     `json:"rate_limit_set,omitempty"`
	Workers      int      `json:"workers,omitempty"`
	MinChunkSize int64    `json:"min_chunk_size,omitempty"`

	// Media Extraction (Persistent)
	//
	// SourceURL is the page URL a media URL was extracted from. Extracted URLs
	// are signed and short-lived, so re-resolving SourceURL is the only way to
	// resume such a download later. FormatID names the chosen format.
	// Request headers are deliberately NOT persisted: they can carry
	// credentials, and a fresh media URL needs freshly resolved headers anyway.
	SourceURL string `json:"source_url,omitempty"`
	FormatID  string `json:"format_id,omitempty"`

	// Parts is non-empty for media that only exists as separate streams (a
	// video track plus an audio track). The engine downloads each part into
	// its own working file and muxes them into DestPath. Part headers are
	// transient for the same reason record headers are: they can carry
	// credentials, and a re-resolved URL needs re-resolved headers.
	Parts []DownloadPart `json:"parts,omitempty"`

	// ManifestURL is set for a fragmented stream (HLS): the engine reads the
	// playlist, downloads every fragment and assembles them into DestPath.
	// Like a part URL it is signed and short-lived, so SourceURL is what makes
	// such a download resumable.
	ManifestURL string `json:"manifest_url,omitempty"`

	// Runtime / Transient Configuration (Not persisted)
	IsResume           bool                 `json:"-" gob:"-"`
	ProgressCh         chan<- DownloadEvent `json:"-" gob:"-"`
	ProgressState      interface{}          `json:"-" gob:"-"` // typically *progress.DownloadProgress
	Runtime            *RuntimeConfig       `json:"-" gob:"-"`
	Headers            map[string]string    `json:"-" gob:"-"`
	Limiter            ByteLimiter          `json:"-" gob:"-"`
	IsExplicitCategory bool                 `json:"-" gob:"-"`
	SupportsRange      bool                 `json:"-" gob:"-"`
}

// DownloadPart is one stream of a multi-part download.
type DownloadPart struct {
	URL      string            `json:"url"`
	FormatID string            `json:"format_id,omitempty"`
	Kind     string            `json:"kind"`
	Size     int64             `json:"size,omitempty"`
	Headers  map[string]string `json:"-" gob:"-"`

	// Complete is set once the stream has been fetched in full. A part's
	// working file cannot be judged by its size, because the concurrent
	// downloader preallocates it, so completion is recorded explicitly and
	// persisted: a resumed multi-part download re-runs only what is missing.
	Complete bool `json:"complete,omitempty"`
}

// Part kinds. A multi-part download carries exactly one of each.
const (
	PartKindVideo = "video"
	PartKindAudio = "audio"
)

// PartWorkingPath is the path a multi-part download uses for one stream. The
// downloaders append IncompleteSuffix themselves, so the file on disk is
// "<final>.p0.video.surge" next to the final file. Both the engine (which
// writes them) and the store's integrity sweep (which must not delete them)
// derive the name from here.
func PartWorkingPath(destPath string, index int, kind string) string {
	if kind == "" {
		kind = fmt.Sprintf("part%d", index)
	}
	return fmt.Sprintf("%s.p%d.%s", destPath, index, kind)
}

// MasterList holds all tracked downloads.
type MasterList struct {
	Downloads []DownloadRecord `json:"downloads"`
}

// DownloadStatus is the transient view returned to the TUI and API clients.
type DownloadStatus struct {
	ID           string  `json:"id"`
	URL          string  `json:"url"`
	Filename     string  `json:"filename"`
	DestPath     string  `json:"dest_path,omitempty"`
	TotalSize    int64   `json:"total_size"`
	Downloaded   int64   `json:"downloaded"`
	Progress     float64 `json:"progress"`
	Speed        float64 `json:"speed"`
	Status       string  `json:"status"`
	Error        string  `json:"error,omitempty"`
	ETA          int64   `json:"eta"`
	Connections  int     `json:"connections"`
	AddedAt      int64   `json:"added_at"`
	TimeTaken    int64   `json:"time_taken"`
	AvgSpeed     float64 `json:"avg_speed"`
	RateLimit    int64   `json:"rate_limit,omitempty"`
	RateLimitSet bool    `json:"rate_limit_set,omitempty"`
}

// CancelResult carries enough metadata for callers to emit lifecycle events
// without creating an import cycle back to the worker pool.
type CancelResult struct {
	Found     bool
	Filename  string
	DestPath  string
	Completed bool
	WasQueued bool
}

type MirrorStatus struct {
	URL    string
	Active bool
	Error  bool
}

// ChunkStatus represents the status of a visualization chunk
type ChunkStatus int

const (
	ChunkPending     ChunkStatus = 0 // 00
	ChunkDownloading ChunkStatus = 1 // 01
	ChunkCompleted   ChunkStatus = 2 // 10 (Bit 2 set)
)
