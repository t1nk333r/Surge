package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/SurgeDM/Surge/internal/mux"
)

// DefaultYtDlpBinary is the executable looked up on PATH. Override it with
// SURGE_YTDLP for a non-standard install.
const DefaultYtDlpBinary = "yt-dlp"

// maxFilenameBytes keeps a derived name well inside the 255-byte limit every
// common filesystem imposes, leaving room for an extension and a "(1)" suffix.
const maxFilenameBytes = 150

// maxExtractorOutput caps the JSON one extraction may return. A 4K video with
// every format listed is a few hundred kilobytes; a hundred times that is a
// page trying to exhaust the daemon.
const maxExtractorOutput = 32 << 20

// YtDlp resolves media URLs through yt-dlp's JSON dump (`yt-dlp -J`).
//
// Only yt-dlp's extractors are used; the download itself stays in Surge. The
// URLs yt-dlp reports for `https` formats are already de-throttled (its
// signature work is done during extraction), and they serve HTTP range
// requests, so the concurrent downloader works on them unchanged.
type YtDlp struct {
	// Binary is the executable name or absolute path.
	Binary string

	lookupOnce sync.Once
	resolved   string
}

// NewYtDlp returns a yt-dlp extractor honouring the SURGE_YTDLP override.
func NewYtDlp() *YtDlp {
	binary := strings.TrimSpace(os.Getenv("SURGE_YTDLP"))
	if binary == "" {
		binary = DefaultYtDlpBinary
	}
	return &YtDlp{Binary: binary}
}

func (y *YtDlp) Name() string { return "yt-dlp" }

// path resolves the binary once; an empty result means "not installed".
func (y *YtDlp) path() string {
	y.lookupOnce.Do(func() {
		binary := strings.TrimSpace(y.Binary)
		if binary == "" {
			binary = DefaultYtDlpBinary
		}
		if strings.ContainsRune(binary, os.PathSeparator) {
			if info, err := os.Stat(binary); err == nil && !info.IsDir() {
				y.resolved = binary
			}
			return
		}
		if found, err := exec.LookPath(binary); err == nil {
			y.resolved = found
		}
	})
	return y.resolved
}

func (y *YtDlp) Available() bool { return y.path() != "" }

// Resolve runs yt-dlp for pageURL and picks the best downloadable form.
func (y *YtDlp) Resolve(ctx context.Context, pageURL string, opts Options) (*Media, error) {
	binary := y.path()
	if binary == "" {
		return nil, ErrNotAvailable
	}

	// A page controls how much JSON yt-dlp prints (titles, descriptions, one
	// entry per format), and the whole thing is decoded in memory, so the
	// daemon's footprint must not be the page's choice.
	stdout := &boundedBuffer{limit: maxExtractorOutput}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, ytDlpArgs(pageURL, opts)...)
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("yt-dlp aborted: %w", ctxErr)
		}
		if stdout.overflowed {
			return nil, fmt.Errorf("yt-dlp returned more than %d bytes of metadata", maxExtractorOutput)
		}
		message := firstLine(stderr.String())
		if isUnsupportedURL(message) {
			return nil, ErrUnsupportedURL
		}
		if message == "" {
			return nil, fmt.Errorf("yt-dlp failed: %w", err)
		}
		return nil, fmt.Errorf("yt-dlp failed: %s", message)
	}
	if stdout.overflowed {
		return nil, fmt.Errorf("yt-dlp returned more than %d bytes of metadata", maxExtractorOutput)
	}

	return parseYtDlpJSON(stdout.buf.Bytes(), pageURL)
}

// boundedBuffer collects a child process's output up to a limit and records
// that it was exceeded, rather than growing to whatever the child writes.
type boundedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			return b.buf.Write(p)
		}
		if _, err := b.buf.Write(p[:room]); err != nil {
			return 0, err
		}
	}
	// Keep accepting the write: killing the pipe would turn a size problem
	// into an opaque "broken pipe" from the child.
	b.overflowed = true
	return len(p), nil
}

// ytDlpArgs builds the argument list for one extraction. It is a separate
// function so the network policy handed to the child process can be asserted
// in a test.
func ytDlpArgs(pageURL string, opts Options) []string {
	args := []string{
		"--dump-single-json",
		"--no-warnings",
		"--no-progress",
		// `--no-playlist` makes a "watch?v=…&list=…" URL resolve to the single
		// video it points at instead of the whole list.
		"--no-playlist",
	}

	// Surge's proxy setting has to be forwarded by hand: yt-dlp is a child
	// process and knows nothing about the transport pool that proxies every
	// in-process request. Without this, a proxied Surge would still resolve
	// media pages over a direct connection — which both leaks the request and
	// hands back CDN URLs signed for the wrong exit address, so the download
	// itself would then fail through the proxy.
	//
	// The user agent is deliberately NOT overridden: an extracted media URL is
	// only valid for the headers yt-dlp reports next to it, and those headers
	// are what the download sends.
	if proxy := strings.TrimSpace(opts.ProxyURL); proxy != "" {
		args = append(args, "--proxy", proxy)
	}

	return append(args, "--", pageURL)
}

type ytFormat struct {
	FormatID       string            `json:"format_id"`
	URL            string            `json:"url"`
	Ext            string            `json:"ext"`
	Protocol       string            `json:"protocol"`
	ACodec         string            `json:"acodec"`
	VCodec         string            `json:"vcodec"`
	Filesize       int64             `json:"filesize"`
	FilesizeApprox int64             `json:"filesize_approx"`
	TBR            float64           `json:"tbr"`
	Height         int               `json:"height"`
	HTTPHeaders    map[string]string `json:"http_headers"`
}

type ytInfo struct {
	Type        string            `json:"_type"`
	Title       string            `json:"title"`
	Ext         string            `json:"ext"`
	URL         string            `json:"url"`
	Protocol    string            `json:"protocol"`
	FormatID    string            `json:"format_id"`
	Filesize    int64             `json:"filesize"`
	HTTPHeaders map[string]string `json:"http_headers"`
	Formats     []ytFormat        `json:"formats"`
	Entries     []json.RawMessage `json:"entries"`
}

// parseYtDlpJSON is the whole decision logic, split out so it can be tested
// against recorded yt-dlp output without running the tool.
func parseYtDlpJSON(data []byte, pageURL string) (*Media, error) {
	var info ytInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("yt-dlp returned data that could not be read: %w", err)
	}

	if info.Type == "playlist" || len(info.Entries) > 0 {
		return nil, ErrPlaylist
	}

	if format, err := selectDirectFormat(&info); err == nil {
		size := format.Filesize
		if size <= 0 {
			size = format.FilesizeApprox
		}
		return &Media{
			URL:       format.URL,
			Headers:   mergeHeaders(info.HTTPHeaders, format.HTTPHeaders),
			Filename:  mediaFilename(info.Title, format.Ext, info.Ext),
			Size:      size,
			FormatID:  format.FormatID,
			SourceURL: pageURL,
		}, nil
	} else if !errors.Is(err, ErrNoDirectFormat) {
		return nil, err
	}

	// No single file exists. Most modern video sites only publish separate
	// video and audio streams, so fall back to the best pair and let the
	// caller download both and mux them.
	if media, err := selectStreamPair(&info, pageURL); err == nil {
		return media, nil
	} else if !errors.Is(err, ErrNoDirectFormat) {
		return nil, err
	}

	// Last resort: a fragmented stream. The engine can assemble one from its
	// playlist, which is still worse than a plain file (no byte ranges, no
	// resume inside a fragment), hence the ordering.
	return selectManifest(&info, pageURL)
}

// selectManifest builds a media item from the best HLS playlist that carries
// both audio and video. A video-only playlist is refused for the same reason a
// video-only file is: the result would be silent, and combining separate
// playlists is not implemented.
func selectManifest(info *ytInfo, pageURL string) (*Media, error) {
	var best *ytFormat
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.URL == "" || !isManifestProtocol(f.Protocol) {
			continue
		}
		if !hasStream(f.VCodec) || !hasStream(f.ACodec) {
			continue
		}
		if best == nil || betterFormat(f, best) {
			best = f
		}
	}
	if best == nil {
		return nil, ErrNoDirectFormat
	}

	// The fragments are joined and remuxed, so the container is the extension
	// the format advertises, defaulting to mp4 — what HLS fragments carry.
	container := strings.TrimPrefix(strings.TrimSpace(best.Ext), ".")
	if container == "" {
		container = "mp4"
	}

	return &Media{
		// Like a parted item, a fragmented one has no single media URL: the
		// page URL is the record's stable identity.
		URL:         pageURL,
		Headers:     mergeHeaders(info.HTTPHeaders, best.HTTPHeaders),
		Filename:    mediaFilename(info.Title, container, container),
		Size:        formatSize(best),
		FormatID:    best.FormatID,
		SourceURL:   pageURL,
		ManifestURL: best.URL,
	}, nil
}

// isManifestProtocol reports whether a format is an HLS playlist Surge can
// assemble. DASH segment lists are a different format and are not supported.
func isManifestProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "m3u8", "m3u8_native":
		return true
	}
	return false
}

// selectStreamPair builds a two-part media item from the best video-only and
// best audio-only formats. Both parts must be plain HTTP files: a fragmented
// stream is not something Surge's engine can fetch, muxing or not.
func selectStreamPair(info *ytInfo, pageURL string) (*Media, error) {
	var bestVideo, bestAudio *ytFormat
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.URL == "" || !isDirectProtocol(f.Protocol) {
			continue
		}
		switch {
		case hasStream(f.VCodec) && !hasStream(f.ACodec):
			if bestVideo == nil || betterFormat(f, bestVideo) {
				bestVideo = f
			}
		case hasStream(f.ACodec) && !hasStream(f.VCodec):
			if bestAudio == nil || betterFormat(f, bestAudio) {
				bestAudio = f
			}
		}
	}

	if bestVideo == nil || bestAudio == nil {
		return nil, ErrNoDirectFormat
	}

	video := Part{
		URL:      bestVideo.URL,
		Headers:  mergeHeaders(info.HTTPHeaders, bestVideo.HTTPHeaders),
		FormatID: bestVideo.FormatID,
		Size:     formatSize(bestVideo),
		Kind:     KindVideo,
		Ext:      bestVideo.Ext,
	}
	audio := Part{
		URL:      bestAudio.URL,
		Headers:  mergeHeaders(info.HTTPHeaders, bestAudio.HTTPHeaders),
		FormatID: bestAudio.FormatID,
		Size:     formatSize(bestAudio),
		Kind:     KindAudio,
		Ext:      bestAudio.Ext,
	}

	container := mux.ContainerFor(video.Ext, audio.Ext)

	return &Media{
		// A parted item has no single media URL. Keeping the page URL here
		// gives the record a stable identity: it is also the store key for
		// resume state, and unlike a signed CDN URL it never changes.
		URL:       pageURL,
		Filename:  mediaFilename(info.Title, container, container),
		Size:      video.Size + audio.Size,
		FormatID:  video.FormatID + "+" + audio.FormatID,
		SourceURL: pageURL,
		Parts:     []Part{video, audio},
	}, nil
}

// selectDirectFormat picks the best format that is a single HTTP file.
//
// A progressive format (audio and video in one stream) is required whenever
// the media has video at all; audio-only media accepts the best audio stream.
// Everything else — HLS/DASH fragment streams and video-only or audio-only
// tracks of a video that would need muxing — is refused, because Surge has no
// muxing step and a silent video is not a successful download.
func selectDirectFormat(info *ytInfo) (*ytFormat, error) {
	if len(info.Formats) == 0 {
		// Extractors for a single media file (a direct image, an mp3 page)
		// report the URL at the top level and carry no format list.
		if info.URL != "" && isDirectProtocol(info.Protocol) {
			return &ytFormat{
				FormatID:    info.FormatID,
				URL:         info.URL,
				Ext:         info.Ext,
				Protocol:    info.Protocol,
				Filesize:    info.Filesize,
				HTTPHeaders: info.HTTPHeaders,
			}, nil
		}
		return nil, ErrNoDirectFormat
	}

	hasVideo := false
	for i := range info.Formats {
		if hasStream(info.Formats[i].VCodec) {
			hasVideo = true
			break
		}
	}

	var best *ytFormat
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.URL == "" || !isDirectProtocol(f.Protocol) {
			continue
		}
		if hasVideo {
			// Needs both streams in the one file.
			if !hasStream(f.VCodec) || !hasStream(f.ACodec) {
				continue
			}
		} else if !hasStream(f.ACodec) {
			continue
		}
		if best == nil || betterFormat(f, best) {
			best = f
		}
	}

	if best == nil {
		return nil, ErrNoDirectFormat
	}
	return best, nil
}

// betterFormat ranks candidates by resolution, then bitrate, then size, so the
// choice is the best quality that is still one file.
func betterFormat(candidate, current *ytFormat) bool {
	if candidate.Height != current.Height {
		return candidate.Height > current.Height
	}
	if candidate.TBR != current.TBR {
		return candidate.TBR > current.TBR
	}
	return formatSize(candidate) > formatSize(current)
}

func formatSize(f *ytFormat) int64 {
	if f.Filesize > 0 {
		return f.Filesize
	}
	return f.FilesizeApprox
}

// hasStream reports whether a codec field names an actual stream. yt-dlp uses
// the string "none" for an absent one, and omits it entirely for some sites.
func hasStream(codec string) bool {
	c := strings.ToLower(strings.TrimSpace(codec))
	return c != "" && c != "none"
}

// isDirectProtocol accepts only the protocols that are one plain HTTP file.
// `m3u8`, `m3u8_native`, `dash`, `http_dash_segments`, `mhtml`, `rtmp`, … are
// all fragment streams.
func isDirectProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "http", "https", "":
		return true
	}
	return false
}

func mergeHeaders(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// mediaFilename builds a filesystem-safe "<title>.<ext>".
func mediaFilename(title, ext, fallbackExt string) string {
	name := SanitizeFilename(title)
	if name == "" {
		return ""
	}
	extension := strings.TrimPrefix(strings.TrimSpace(ext), ".")
	if extension == "" {
		extension = strings.TrimPrefix(strings.TrimSpace(fallbackExt), ".")
	}
	extension = SanitizeFilename(extension)
	if extension == "" {
		return name
	}
	return name + "." + extension
}

// SanitizeFilename strips path separators and control characters, plus the
// characters Windows refuses, and trims to a length every filesystem accepts.
func SanitizeFilename(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 || r == 0x7f || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, value)

	cleaned = strings.Trim(strings.TrimSpace(cleaned), ". ")
	// Truncate on a rune boundary: half a multi-byte character is not a
	// character, and some filesystems reject invalid UTF-8 outright.
	if len(cleaned) > maxFilenameBytes {
		cut := maxFilenameBytes
		for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
			cut--
		}
		cleaned = strings.TrimSpace(cleaned[:cut])
	}
	if cleaned == "." || cleaned == ".." {
		return ""
	}
	return cleaned
}

func firstLine(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		trimmed = trimmed[:i]
	}
	return strings.TrimSpace(trimmed)
}

func isUnsupportedURL(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "unsupported url") ||
		strings.Contains(lower, "is not a valid url")
}
