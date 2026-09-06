// Package extractor resolves a media page URL (YouTube, Vimeo, …) into the
// direct HTTP URL of a media file that Surge's own download engine can fetch.
//
// The engine is never bypassed: an extractor only answers the question "which
// bytes should we download, and with which headers", so segmented downloads,
// rate limiting, resume, mirrors and progress reporting all keep working
// exactly as they do for a plain URL. Anything an extractor cannot express as
// one HTTP URL (HLS/DASH fragment streams, separate audio+video tracks that
// need muxing, playlists) is reported as an error rather than downloaded
// partially or wrongly.
package extractor

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrNotAvailable means the backing tool is not installed.
	ErrNotAvailable = errors.New("extractor not available")

	// ErrUnsupportedURL means the tool has no extractor for this site.
	ErrUnsupportedURL = errors.New("no extractor for this URL")

	// ErrPlaylist means the URL identifies a collection, not one media item.
	// Surge downloads one file per record, so the caller must expand the
	// collection into individual requests.
	ErrPlaylist = errors.New("URL is a playlist; add its items individually")

	// ErrNoDirectFormat means the site only offers fragmented (HLS/DASH) or
	// split audio/video formats, neither of which is a single HTTP file.
	ErrNoDirectFormat = errors.New("no single-file format available for this URL")
)

// Media is a resolved, directly downloadable media file.
type Media struct {
	// URL is the direct media URL. These are frequently signed, time-limited
	// and bound to the requesting IP, which is why SourceURL is kept.
	URL string

	// Headers must accompany every request for URL (User-Agent above all).
	Headers map[string]string

	// Filename is the suggested name, derived from the media title.
	Filename string

	// Size is the reported size in bytes, or 0 when the site does not say.
	Size int64

	// FormatID identifies the chosen format to the extractor, so the same
	// format can be re-resolved later.
	FormatID string

	// SourceURL is the page URL this was resolved from. Re-resolving it is the
	// only way to recover from an expired media URL.
	SourceURL string

	// Parts is non-empty when the media only exists as separate streams that
	// must be muxed after download (a video track plus an audio track). URL,
	// Size and FormatID then describe the whole media rather than one request:
	// URL is the page URL, Size the sum of the parts.
	Parts []Part

	// ManifestURL is set when the media is only offered as a fragmented HLS
	// stream. URL then holds the page URL and Size is an estimate at best.
	ManifestURL string
}

// Kind labels a stream inside a multi-part media item.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Part is one stream of a multi-part media item.
type Part struct {
	URL      string
	Headers  map[string]string
	FormatID string
	Size     int64
	Kind     Kind

	// Ext is the stream's own container extension, which decides what the
	// muxed output can be.
	Ext string
}

// Options carries the network policy an extractor must obey. Extractors run
// as child processes, so they are outside the shared transport pool that
// applies Surge's proxy to every in-process request: the settings have to be
// handed to them explicitly or a proxied Surge would leak direct connections.
type Options struct {
	// ProxyURL is Surge's configured proxy (Network.ProxyURL). Empty means
	// "inherit the environment", which is what the transport pool does too
	// (http.ProxyFromEnvironment).
	ProxyURL string
}

// Extractor resolves a page URL into a directly downloadable media file.
type Extractor interface {
	// Name identifies the extractor in logs and errors.
	Name() string

	// Available reports whether the extractor can run at all (tool installed).
	Available() bool

	// Resolve returns the best downloadable form of pageURL, or one of the
	// package's sentinel errors. opts carries the network policy the
	// extractor must honour.
	Resolve(ctx context.Context, pageURL string, opts Options) (*Media, error)
}

// LooksExtractable reports whether a probe result suggests a media *page*
// rather than a file: Surge would otherwise happily save the HTML.
//
// This is deliberately content-type driven rather than a list of known hosts:
// any site the installed tool supports works, and a normal file download never
// pays the cost of starting the tool.
func LooksExtractable(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}
