// Package hls parses HTTP Live Streaming playlists (RFC 8216) so Surge can
// download a fragmented stream with its own engine instead of refusing it.
//
// The package is deliberately pure: it turns playlist bytes into a list of
// segment URLs and never fetches, spawns or writes anything. Every byte a user
// ends up with is therefore still moved by Surge's download engine, which keeps
// segmenting, rate limiting, resume, mirrors and progress reporting working for
// HLS exactly as they work for a plain URL. It also makes the tricky part of
// HLS - variant selection, byte ranges, initialisation segments - testable
// against fixtures with no network at all.
//
// What cannot be downloaded correctly is refused up front rather than saved as
// a broken file: encrypted streams (Surge would write ciphertext), and live
// streams (no size, no end, so no "download" to complete).
package hls

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

var (
	// ErrNotPlaylist means the data does not start with #EXTM3U, so it is not
	// a playlist at all (usually an error page served with a .m3u8 name).
	ErrNotPlaylist = errors.New("not an HLS playlist")

	// ErrLive means a media playlist has no #EXT-X-ENDLIST. Such a stream
	// keeps growing, so it has neither a total size nor a point at which the
	// download is finished; recording it is a different feature from
	// downloading a file.
	ErrLive = errors.New("live HLS stream; not a finite download")

	// ErrEncrypted means the segments are protected by #EXT-X-KEY with a
	// method other than NONE. Surge downloads bytes as served, so it would
	// save the encrypted segments verbatim and produce a file no player can
	// open; decryption needs the key exchange (and often DRM) that belongs to
	// the site's player, not to a download manager.
	ErrEncrypted = errors.New("encrypted HLS stream")

	// ErrEmptyPlaylist means the playlist parsed cleanly but declares neither
	// variants nor segments, so there is nothing to download.
	ErrEmptyPlaylist = errors.New("playlist has no variants and no segments")

	// ErrInvalidBaseURL means baseURL is not an absolute URL. Relative segment
	// URIs could not be resolved, and guessing a base would fabricate
	// download targets.
	ErrInvalidBaseURL = errors.New("playlist base URL is not absolute")
)

// playlistContentTypes are the MIME types servers use for HLS playlists.
var playlistContentTypes = map[string]bool{
	"application/vnd.apple.mpegurl": true,
	"application/x-mpegurl":         true,
	"audio/mpegurl":                 true,
	"audio/x-mpegurl":               true,
	"application/mpegurl":           true,
}

// LooksLikePlaylist reports whether a probed resource is an HLS playlist
// rather than a media file, so a caller can assemble the stream instead of
// saving a few kilobytes of text. The URL is consulted as well because plenty
// of CDNs serve playlists as text/plain or application/octet-stream.
func LooksLikePlaylist(contentType, rawurl string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if playlistContentTypes[ct] {
		return true
	}

	parsed, err := url.Parse(rawurl)
	if err != nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	return strings.HasSuffix(path, ".m3u8") || strings.HasSuffix(path, ".m3u")
}

// Playlist is a parsed master or media playlist. Exactly one of Variants and
// Segments is non-empty for a well-formed playlist.
type Playlist struct {
	// Variants is non-empty for a master playlist.
	Variants []Variant

	// AudioRenditions holds the EXT-X-MEDIA TYPE=AUDIO groups of a master
	// playlist. A variant that references one of these groups carries no audio
	// in its own segments, so downloading the variant alone would produce a
	// silent video.
	AudioRenditions []Rendition

	// Segments is non-empty for a media playlist.
	Segments []Segment

	// InitSection is the EXT-X-MAP initialisation segment (fMP4 streams);
	// it must be written before the first segment.
	InitSection *Segment

	TargetDuration float64
	TotalDuration  float64 // sum of segment durations

	// Live reports the absence of EXT-X-ENDLIST. Parse refuses live playlists
	// with ErrLive, so a Playlist returned by Parse always has Live == false;
	// the field names the state the parser detected.
	Live bool
}

// Variant is one quality level of a master playlist.
type Variant struct {
	URL        string
	Bandwidth  int    // BANDWIDTH attribute, bits per second
	Resolution string // e.g. "1920x1080", empty when absent
	Codecs     string
	Height     int // parsed from Resolution, 0 when absent

	// Audio is the STREAM-INF AUDIO attribute: the id of the audio rendition
	// group this variant expects its sound to come from. Non-empty means the
	// audio lives in a separate rendition (see Playlist.AudioRenditions) and
	// the variant's own segments are probably video-only.
	Audio string
}

// Rendition is an EXT-X-MEDIA alternative track of a master playlist.
type Rendition struct {
	GroupID  string
	Name     string
	Language string

	// URL is empty when the rendition has no URI attribute, which means the
	// track is already multiplexed into the segments of the variants that
	// reference its group.
	URL string

	Default bool
}

// Segment is one media segment, or the initialisation section.
type Segment struct {
	URL      string
	Duration float64
	// Offset/Length carry EXT-X-BYTERANGE; Length == 0 means "whole file".
	Offset int64
	Length int64
}

// Playlist tags this package acts on. Everything else is ignored, which is what
// the spec asks clients to do with tags they do not implement.
const (
	tagHeader         = "#EXTM3U"
	tagInf            = "#EXTINF"
	tagStreamInf      = "#EXT-X-STREAM-INF"
	tagMedia          = "#EXT-X-MEDIA"
	tagByteRange      = "#EXT-X-BYTERANGE"
	tagMap            = "#EXT-X-MAP"
	tagKey            = "#EXT-X-KEY"
	tagTargetDuration = "#EXT-X-TARGETDURATION"
	tagEndList        = "#EXT-X-ENDLIST"
)

// byteRange is a parsed EXT-X-BYTERANGE value. hasOffset distinguishes an
// explicit "@0" from an omitted offset, which mean different things.
type byteRange struct {
	length    int64
	offset    int64
	hasOffset bool
}

// Parse reads a master or media playlist. Relative URIs are resolved against
// baseURL, which must be the absolute URL the playlist was fetched from.
//
// Parse either returns a complete playlist or an error: a partially parsed
// playlist is never returned, because a caller that downloaded "most" of the
// segments would write a truncated file and call it done.
func Parse(data []byte, baseURL string) (*Playlist, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrInvalidBaseURL, baseURL, err)
	}
	if !base.IsAbs() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidBaseURL, baseURL)
	}

	// A UTF-8 BOM is legal in a playlist served as text; it would otherwise
	// hide the #EXTM3U header behind three invisible bytes.
	text := strings.TrimPrefix(string(data), "\ufeff")

	p := &Playlist{}
	var (
		seenHeader     bool
		endList        bool
		pendingVariant *Variant
		pendingRange   *byteRange
		pendingDur     float64
		havePendingDur bool
		mapRaw         string
		haveMap        bool
		// nextOffset tracks, per media resource, the byte just past the last
		// sub-range taken from it, which is where a BYTERANGE without an
		// explicit offset continues.
		nextOffset = map[string]int64{}
	)

	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw) // also drops the \r of CRLF playlists
		if line == "" {
			continue
		}

		if !seenHeader {
			if line != tagHeader {
				return nil, fmt.Errorf("%w: first line is %q", ErrNotPlaylist, line)
			}
			seenHeader = true
			continue
		}

		if strings.HasPrefix(line, "#") {
			// Plain comments start with '#' but are not tags.
			if !strings.HasPrefix(line, "#EXT") {
				continue
			}

			tag, value := splitTag(line)
			switch tag {
			case tagStreamInf:
				attrs := parseAttributes(value)
				v := variantFromAttrs(attrs)
				pendingVariant = &v

			case tagMedia:
				// Only audio renditions are recorded: they decide whether a
				// variant needs a second stream to have sound. TYPE=SUBTITLES
				// and TYPE=CLOSED-CAPTIONS are ignored (Surge downloads media,
				// not sidecar text tracks), and so is
				// EXT-X-I-FRAME-STREAM-INF, whose trick-play I-frame playlists
				// are not watchable video.
				a := parseAttributes(value)
				if !strings.EqualFold(a["TYPE"], "AUDIO") {
					continue
				}
				r, err := renditionFromAttrs(base, a)
				if err != nil {
					return nil, fmt.Errorf("line %d: %s: %w", lineNo, tagMedia, err)
				}
				p.AudioRenditions = append(p.AudioRenditions, r)

			case tagInf:
				// "#EXTINF:<duration>,<title>": the title is free text and
				// unused, but the comma is what terminates the duration.
				durText := value
				if comma := strings.IndexByte(durText, ','); comma >= 0 {
					durText = durText[:comma]
				}
				d, err := strconv.ParseFloat(strings.TrimSpace(durText), 64)
				if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
					return nil, fmt.Errorf("line %d: %s: invalid duration %q", lineNo, tagInf, durText)
				}
				pendingDur = d
				havePendingDur = true

			case tagByteRange:
				br, err := parseByteRange(value)
				if err != nil {
					return nil, fmt.Errorf("line %d: %s: %w", lineNo, tagByteRange, err)
				}
				pendingRange = &br

			case tagMap:
				if haveMap {
					// One init section per download. A second, different
					// EXT-X-MAP means the stream changes its container
					// mid-way, which cannot be written as one playable file
					// here, so it is refused instead of silently truncated.
					if value != mapRaw {
						return nil, fmt.Errorf("line %d: %s changes mid-playlist; unsupported", lineNo, tagMap)
					}
					continue
				}
				init, err := initSectionFromAttrs(base, parseAttributes(value), nextOffset)
				if err != nil {
					return nil, fmt.Errorf("line %d: %s: %w", lineNo, tagMap, err)
				}
				p.InitSection = init
				mapRaw, haveMap = value, true

			case tagKey:
				// A missing METHOD is malformed; refusing is the safe reading,
				// since the alternative is saving bytes that may be encrypted.
				if method := strings.ToUpper(attrs(value, "METHOD")); method != "NONE" {
					return nil, fmt.Errorf("%w: METHOD=%q", ErrEncrypted, method)
				}

			case tagTargetDuration:
				// The spec says integer seconds; parsed as a float so a
				// non-conforming "10.0" does not fail the whole download.
				d, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
					return nil, fmt.Errorf("line %d: %s: invalid duration %q", lineNo, tagTargetDuration, value)
				}
				p.TargetDuration = d

			case tagEndList:
				endList = true
			}
			continue
		}

		// Not a tag: a URI line, which belongs to whichever tag preceded it.
		resolved, err := resolveURI(base, line)
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid URI %q: %v", lineNo, line, err)
		}

		switch {
		case pendingVariant != nil:
			pendingVariant.URL = resolved
			p.Variants = append(p.Variants, *pendingVariant)
			pendingVariant = nil

		case havePendingDur:
			seg := Segment{URL: resolved, Duration: pendingDur}
			if pendingRange != nil {
				offset, length, rangeErr := pendingRange.resolve(resolved, nextOffset)
				if rangeErr != nil {
					return nil, fmt.Errorf("line %d: %s: %w", lineNo, tagByteRange, rangeErr)
				}
				seg.Offset, seg.Length = offset, length
			}
			p.Segments = append(p.Segments, seg)
			havePendingDur, pendingDur, pendingRange = false, 0, nil

		default:
			// A URI with no EXTINF and no STREAM-INF is not a segment the
			// spec defines; skipping it keeps one stray line from failing an
			// otherwise good playlist.
		}
	}

	if !seenHeader {
		return nil, fmt.Errorf("%w: no %s header", ErrNotPlaylist, tagHeader)
	}
	if len(p.Variants) == 0 && len(p.Segments) == 0 {
		return nil, ErrEmptyPlaylist
	}

	// Only a media playlist can end; a master playlist has no EXT-X-ENDLIST.
	p.Live = len(p.Segments) > 0 && !endList
	if p.Live {
		return nil, ErrLive
	}

	for _, s := range p.Segments {
		p.TotalDuration += s.Duration
	}
	return p, nil
}

// BestVariant returns the highest-quality variant (height first, then
// bandwidth), or nil for a media playlist.
//
// Height wins over bandwidth because a user asking for "the video" wants the
// most pixels; bandwidth only breaks ties between equal resolutions, where it
// stands for the better encode.
func BestVariant(p *Playlist) *Variant {
	if p == nil {
		return nil
	}
	var best *Variant
	for i := range p.Variants {
		v := &p.Variants[i]
		if v.URL == "" {
			continue // nothing to download
		}
		if best == nil || v.Height > best.Height ||
			(v.Height == best.Height && v.Bandwidth > best.Bandwidth) {
			best = v
		}
	}
	return best
}

// EstimatedSize returns the expected byte size of a media playlist given the
// variant bandwidth it came from, or 0 when it cannot be estimated.
//
// This is a progress estimate only: BANDWIDTH is the peak bit rate the encoder
// promises, so the figure is an upper bound that drifts from reality by the
// difference between peak and average rate. The true size is known once the
// segments have been downloaded, so callers must treat 0 and any value here as
// display hints, never as an allocation or completion criterion.
func EstimatedSize(p *Playlist, bandwidthBitsPerSecond int) int64 {
	if p == nil || bandwidthBitsPerSecond <= 0 || p.TotalDuration <= 0 {
		return 0
	}
	return int64(math.Round(float64(bandwidthBitsPerSecond) / 8 * p.TotalDuration))
}

// resolve turns a pending EXT-X-BYTERANGE into a concrete offset and length for
// segURL, advancing the per-resource continuation point.
func (br byteRange) resolve(segURL string, nextOffset map[string]int64) (offset, length int64, err error) {
	offset = br.offset
	if !br.hasOffset {
		// RFC 8216 4.3.2.2: without an offset the sub-range starts at the byte
		// after the previous sub-range of the same resource. Tracking it per
		// resource URL is what makes a chain of bare "#EXT-X-BYTERANGE:<len>"
		// lines address consecutive parts of one file instead of re-reading
		// its first bytes over and over.
		offset = nextOffset[segURL]
	}
	if offset > math.MaxInt64-br.length {
		return 0, 0, fmt.Errorf("byte range %d@%d exceeds the addressable range", br.length, offset)
	}
	nextOffset[segURL] = offset + br.length
	return offset, br.length, nil
}

// splitTag splits "#EXT-X-TAG:value" into tag and value. A tag without a colon
// (e.g. #EXT-X-ENDLIST) yields an empty value.
func splitTag(line string) (tag, value string) {
	if colon := strings.IndexByte(line, ':'); colon >= 0 {
		return line[:colon], line[colon+1:]
	}
	return line, ""
}

// parseAttributes parses an HLS attribute list (RFC 8216 4.2) into
// upper-cased names mapped to unquoted values.
//
// The list cannot be split on commas: quoted values legitimately contain them
// (CODECS="avc1.640028,mp4a.40.2" is the common case), and a naive split turns
// one variant's codec list into two bogus attributes. The scan therefore tracks
// whether it is inside a quoted string and only treats commas outside quotes as
// separators.
func parseAttributes(list string) map[string]string {
	out := make(map[string]string)
	for _, field := range splitOutsideQuotes(list) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		eq := strings.IndexByte(field, '=')
		if eq < 0 {
			continue // not an attribute; nothing to record
		}
		name := strings.ToUpper(strings.TrimSpace(field[:eq]))
		value := strings.TrimSpace(field[eq+1:])
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		out[name] = value
	}
	return out
}

// attrs reads a single attribute, for the cases where building the whole map
// would be wasted work.
func attrs(list, name string) string {
	return parseAttributes(list)[name]
}

// splitOutsideQuotes splits on commas that are not inside a quoted string.
func splitOutsideQuotes(s string) []string {
	var (
		out      []string
		inQuotes bool
		start    int
	)
	for i := range len(s) {
		switch s[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func variantFromAttrs(a map[string]string) Variant {
	v := Variant{
		Resolution: a["RESOLUTION"],
		Codecs:     a["CODECS"],
		Audio:      a["AUDIO"],
	}
	// A malformed BANDWIDTH only costs the tie-break, so it is not fatal.
	v.Bandwidth, _ = strconv.Atoi(strings.TrimSpace(a["BANDWIDTH"]))
	v.Height = heightOf(v.Resolution)
	return v
}

// heightOf pulls the vertical pixel count out of a "<width>x<height>"
// RESOLUTION value; 0 means "unknown", which sorts below every real height.
func heightOf(resolution string) int {
	x := strings.IndexAny(resolution, "xX")
	if x < 0 {
		return 0
	}
	h, err := strconv.Atoi(strings.TrimSpace(resolution[x+1:]))
	if err != nil || h < 0 {
		return 0
	}
	return h
}

func renditionFromAttrs(base *url.URL, a map[string]string) (Rendition, error) {
	r := Rendition{
		GroupID:  a["GROUP-ID"],
		Name:     a["NAME"],
		Language: a["LANGUAGE"],
		Default:  strings.EqualFold(a["DEFAULT"], "YES"),
	}
	// URI is absent when the track is muxed into the video segments; that is a
	// fact about the stream, not an error.
	if uri := a["URI"]; uri != "" {
		resolved, err := resolveURI(base, uri)
		if err != nil {
			return Rendition{}, fmt.Errorf("invalid URI %q: %v", uri, err)
		}
		r.URL = resolved
	}
	return r, nil
}

func initSectionFromAttrs(base *url.URL, a map[string]string, nextOffset map[string]int64) (*Segment, error) {
	uri := a["URI"]
	if uri == "" {
		return nil, errors.New("missing URI attribute")
	}
	resolved, err := resolveURI(base, uri)
	if err != nil {
		return nil, fmt.Errorf("invalid URI %q: %v", uri, err)
	}
	init := &Segment{URL: resolved}
	if rawRange := a["BYTERANGE"]; rawRange != "" {
		br, err := parseByteRange(rawRange)
		if err != nil {
			return nil, err
		}
		// The init section is a sub-range like any other, so it moves the
		// continuation point for segments that follow in the same resource.
		offset, length, rangeErr := br.resolve(resolved, nextOffset)
		if rangeErr != nil {
			return nil, rangeErr
		}
		init.Offset, init.Length = offset, length
	}
	return init, nil
}

// parseByteRange parses "<length>[@<offset>]".
func parseByteRange(value string) (byteRange, error) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	lengthText, offsetText := value, ""
	if at := strings.IndexByte(value, '@'); at >= 0 {
		lengthText, offsetText = value[:at], value[at+1:]
	}
	length, err := strconv.ParseInt(strings.TrimSpace(lengthText), 10, 64)
	if err != nil || length < 0 {
		return byteRange{}, fmt.Errorf("invalid length %q", lengthText)
	}
	br := byteRange{length: length}
	if offsetText != "" {
		offset, err := strconv.ParseInt(strings.TrimSpace(offsetText), 10, 64)
		if err != nil || offset < 0 {
			return byteRange{}, fmt.Errorf("invalid offset %q", offsetText)
		}
		br.offset, br.hasOffset = offset, true
	}
	// The pair becomes an HTTP range request, so a sum that wraps would ask
	// for a negative end and fetch something other than what was declared.
	if br.offset > math.MaxInt64-br.length {
		return byteRange{}, fmt.Errorf("byte range %d@%d exceeds the addressable range", br.length, br.offset)
	}
	return br, nil
}

// resolveURI resolves a playlist URI against the playlist's own URL. Absolute
// URIs are passed through untouched so a CDN's exact form survives.
func resolveURI(base *url.URL, ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		return ref, nil
	}
	return base.ResolveReference(u).String(), nil
}
