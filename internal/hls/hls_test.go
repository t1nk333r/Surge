package hls

import (
	"errors"
	"testing"
)

const baseURL = "https://cdn.example.com/media/1234/master.m3u8"

// A master playlist shaped like the ones a real CDN serves: an audio rendition
// group, video-only variants that reference it, an absolute URI on the CDN
// overflow host, a quoted CODECS list containing a comma, and the trick-play
// and subtitle tags a client is expected to ignore.
const masterPlaylist = "\ufeff" + `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-INDEPENDENT-SEGMENTS

# Audio, one group, two languages.
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac-128k",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="audio/en/128k.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac-128k",NAME="Deutsch, Stereo",LANGUAGE="de",DEFAULT=NO,AUTOSELECT=YES,URI="audio/de/128k.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",URI="subs/en.m3u8"

#EXT-X-STREAM-INF:BANDWIDTH=1400000,AVERAGE-BANDWIDTH=1200000,RESOLUTION=854x480,CODECS="avc1.4d401f",AUDIO="aac-128k",SUBTITLES="subs"
v0/480p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5200000,AVERAGE-BANDWIDTH=4800000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="aac-128k"
v0/1080p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=7800000,AVERAGE-BANDWIDTH=7100000,RESOLUTION=1920x1080,CODECS="avc1.640028",AUDIO="aac-128k"
https://overflow.example.net/media/1234/v1/1080p-high.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=560000,CODECS="mp4a.40.2"
audio-only.m3u8

#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=95000,RESOLUTION=1920x1080,CODECS="avc1.640028",URI="v0/iframe.m3u8"
`

// A finished fMP4 media playlist: one initialisation section and segments that
// are byte ranges of two resources. The third segment omits its offset, so it
// must continue where the second one ended inside the same file.
const mediaPlaylist = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-KEY:METHOD=NONE
#EXT-X-MAP:URI="v0/1080p.mp4",BYTERANGE="1024@0"

#EXTINF:6.000,
#EXT-X-BYTERANGE:75000@1024
v0/1080p.mp4
#EXTINF:6.000,segment two
#EXT-X-BYTERANGE:81000@76024
v0/1080p.mp4
#EXTINF:5.500,
#EXT-X-BYTERANGE:64000
v0/1080p.mp4
#EXTINF:4.250,
../shared/tail.mp4
#EXT-X-ENDLIST
`

func TestParseMasterPlaylistOffersEveryVariantWithItsQuality(t *testing.T) {
	p, err := Parse([]byte(masterPlaylist), baseURL)
	if err != nil {
		t.Fatalf("Parse of a valid master playlist failed: %v", err)
	}

	if len(p.Segments) != 0 {
		t.Errorf("master playlist reported %d segments; a caller must not try to download them", len(p.Segments))
	}
	if p.InitSection != nil {
		t.Errorf("master playlist reported an init section %+v; there is none", p.InitSection)
	}

	want := []Variant{
		{
			URL:        "https://cdn.example.com/media/1234/v0/480p.m3u8",
			Bandwidth:  1400000,
			Resolution: "854x480",
			Codecs:     "avc1.4d401f",
			Height:     480,
			Audio:      "aac-128k",
		},
		{
			URL:        "https://cdn.example.com/media/1234/v0/1080p.m3u8",
			Bandwidth:  5200000,
			Resolution: "1920x1080",
			// The comma inside the quoted CODECS list must survive parsing.
			Codecs: "avc1.640028,mp4a.40.2",
			Height: 1080,
			Audio:  "aac-128k",
		},
		{
			// Absolute URIs point at another host and must pass through.
			URL:        "https://overflow.example.net/media/1234/v1/1080p-high.m3u8",
			Bandwidth:  7800000,
			Resolution: "1920x1080",
			Codecs:     "avc1.640028",
			Height:     1080,
			Audio:      "aac-128k",
		},
		{
			// No RESOLUTION: an audio-only variant has no height to sort by.
			URL:       "https://cdn.example.com/media/1234/audio-only.m3u8",
			Bandwidth: 560000,
			Codecs:    "mp4a.40.2",
		},
	}

	if len(p.Variants) != len(want) {
		t.Fatalf("got %d variants, want %d (I-FRAME and MEDIA tags must not become variants): %+v",
			len(p.Variants), len(want), p.Variants)
	}
	for i, w := range want {
		if p.Variants[i] != w {
			t.Errorf("variant %d = %+v, want %+v", i, p.Variants[i], w)
		}
	}
}

func TestParseMasterPlaylistNamesTheAudioAVariantNeeds(t *testing.T) {
	p, err := Parse([]byte(masterPlaylist), baseURL)
	if err != nil {
		t.Fatalf("Parse of a valid master playlist failed: %v", err)
	}

	want := []Rendition{
		{
			GroupID:  "aac-128k",
			Name:     "English",
			Language: "en",
			URL:      "https://cdn.example.com/media/1234/audio/en/128k.m3u8",
			Default:  true,
		},
		{
			GroupID: "aac-128k",
			// A quoted NAME may contain a comma just like CODECS does.
			Name:     "Deutsch, Stereo",
			Language: "de",
			URL:      "https://cdn.example.com/media/1234/audio/de/128k.m3u8",
			Default:  false,
		},
	}

	if len(p.AudioRenditions) != len(want) {
		t.Fatalf("got %d audio renditions, want %d (subtitle renditions must be ignored): %+v",
			len(p.AudioRenditions), len(want), p.AudioRenditions)
	}
	for i, w := range want {
		if p.AudioRenditions[i] != w {
			t.Errorf("audio rendition %d = %+v, want %+v", i, p.AudioRenditions[i], w)
		}
	}

	// The point of all this: a caller can tell that downloading the best
	// variant alone would produce a silent video.
	best := BestVariant(p)
	if best == nil {
		t.Fatal("BestVariant returned nil for a master playlist")
	}
	if best.Audio != "aac-128k" {
		t.Errorf("best variant Audio = %q, want the group id %q so the caller knows to fetch a separate audio stream",
			best.Audio, "aac-128k")
	}
}

func TestParseRecordsMuxedAudioRenditionWithoutURL(t *testing.T) {
	// A rendition with no URI describes audio already inside the video
	// segments; the caller must not fetch a second stream for it.
	const playlist = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="muxed",NAME="Main",LANGUAGE="en",DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="muxed"
360p.m3u8
`
	p, err := Parse([]byte(playlist), baseURL)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(p.AudioRenditions) != 1 {
		t.Fatalf("got %d audio renditions, want 1: %+v", len(p.AudioRenditions), p.AudioRenditions)
	}
	if got := p.AudioRenditions[0].URL; got != "" {
		t.Errorf("muxed rendition URL = %q, want empty so no separate audio download is attempted", got)
	}
	if !p.AudioRenditions[0].Default {
		t.Error("DEFAULT=YES rendition reported Default = false")
	}
}

func TestBestVariantPrefersHeightThenBandwidth(t *testing.T) {
	tests := []struct {
		name     string
		variants []Variant
		wantURL  string
	}{
		{
			name: "taller wins even at lower bandwidth",
			variants: []Variant{
				{URL: "a", Height: 720, Bandwidth: 9000000},
				{URL: "b", Height: 1080, Bandwidth: 4000000},
			},
			wantURL: "b",
		},
		{
			name: "equal height falls back to bandwidth",
			variants: []Variant{
				{URL: "a", Height: 1080, Bandwidth: 5200000},
				{URL: "b", Height: 1080, Bandwidth: 7800000},
				{URL: "c", Height: 1080, Bandwidth: 3000000},
			},
			wantURL: "b",
		},
		{
			name: "variant without a URL is not downloadable and is skipped",
			variants: []Variant{
				{URL: "", Height: 2160, Bandwidth: 20000000},
				{URL: "a", Height: 480, Bandwidth: 1400000},
			},
			wantURL: "a",
		},
		{
			name:     "unknown heights compare by bandwidth alone",
			variants: []Variant{{URL: "a", Bandwidth: 560000}, {URL: "b", Bandwidth: 1400000}},
			wantURL:  "b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BestVariant(&Playlist{Variants: tc.variants})
			if got == nil {
				t.Fatalf("BestVariant returned nil, want the variant %q", tc.wantURL)
			}
			if got.URL != tc.wantURL {
				t.Errorf("BestVariant chose %q, want %q", got.URL, tc.wantURL)
			}
		})
	}
}

func TestBestVariantIsNilWhenThereIsNothingToChoose(t *testing.T) {
	if got := BestVariant(nil); got != nil {
		t.Errorf("BestVariant(nil) = %+v, want nil", got)
	}
	media := &Playlist{Segments: []Segment{{URL: "s0.ts", Duration: 6}}}
	if got := BestVariant(media); got != nil {
		t.Errorf("BestVariant of a media playlist = %+v, want nil (a media playlist has no variants)", got)
	}
	empty := &Playlist{Variants: []Variant{{URL: "", Height: 1080}}}
	if got := BestVariant(empty); got != nil {
		t.Errorf("BestVariant with only URL-less variants = %+v, want nil", got)
	}
}

func TestBestVariantPicksTheHighestQualityOfARealMaster(t *testing.T) {
	p, err := Parse([]byte(masterPlaylist), baseURL)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	best := BestVariant(p)
	if best == nil {
		t.Fatal("BestVariant returned nil for a master playlist with four variants")
	}
	const wantURL = "https://overflow.example.net/media/1234/v1/1080p-high.m3u8"
	if best.URL != wantURL {
		t.Errorf("BestVariant URL = %q, want %q (1080p, highest bandwidth)", best.URL, wantURL)
	}
}

func TestParseMediaPlaylistYieldsExactSegmentsToDownload(t *testing.T) {
	p, err := Parse([]byte(mediaPlaylist), baseURL)
	if err != nil {
		t.Fatalf("Parse of a finished media playlist failed: %v", err)
	}

	if len(p.Variants) != 0 {
		t.Errorf("media playlist reported %d variants, want 0", len(p.Variants))
	}
	if p.Live {
		t.Error("playlist with EXT-X-ENDLIST reported Live = true")
	}
	if p.TargetDuration != 6 {
		t.Errorf("TargetDuration = %v, want 6", p.TargetDuration)
	}
	if p.TotalDuration != 21.75 {
		t.Errorf("TotalDuration = %v, want 21.75 (6+6+5.5+4.25)", p.TotalDuration)
	}

	wantInit := Segment{URL: "https://cdn.example.com/media/1234/v0/1080p.mp4", Offset: 0, Length: 1024}
	if p.InitSection == nil {
		t.Fatalf("InitSection is nil; EXT-X-MAP must be downloaded before the first segment")
	}
	if *p.InitSection != wantInit {
		t.Errorf("InitSection = %+v, want %+v", *p.InitSection, wantInit)
	}

	want := []Segment{
		{URL: "https://cdn.example.com/media/1234/v0/1080p.mp4", Duration: 6, Offset: 1024, Length: 75000},
		{URL: "https://cdn.example.com/media/1234/v0/1080p.mp4", Duration: 6, Offset: 76024, Length: 81000},
		{
			// BYTERANGE without an offset continues after the previous
			// sub-range of the same resource: 76024 + 81000.
			URL: "https://cdn.example.com/media/1234/v0/1080p.mp4", Duration: 5.5, Offset: 157024, Length: 64000,
		},
		{
			// No BYTERANGE at all: whole file, and the relative URI walks up
			// out of the playlist's directory.
			URL: "https://cdn.example.com/media/shared/tail.mp4", Duration: 4.25, Offset: 0, Length: 0,
		},
	}
	if len(p.Segments) != len(want) {
		t.Fatalf("got %d segments, want %d: %+v", len(p.Segments), len(want), p.Segments)
	}
	for i, w := range want {
		if p.Segments[i] != w {
			t.Errorf("segment %d = %+v, want %+v", i, p.Segments[i], w)
		}
	}
}

func TestParseByteRangeContinuationIsPerResource(t *testing.T) {
	// Two interleaved resources: each bare BYTERANGE must continue after the
	// last range of its own file, not after whatever came immediately before.
	const playlist = `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
#EXT-X-BYTERANGE:100@0
a.mp4
#EXTINF:2.0,
#EXT-X-BYTERANGE:200@0
b.mp4
#EXTINF:2.0,
#EXT-X-BYTERANGE:300
a.mp4
#EXTINF:2.0,
#EXT-X-BYTERANGE:400
b.mp4
#EXT-X-ENDLIST
`
	p, err := Parse([]byte(playlist), baseURL)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	want := []Segment{
		{URL: "https://cdn.example.com/media/1234/a.mp4", Duration: 2, Offset: 0, Length: 100},
		{URL: "https://cdn.example.com/media/1234/b.mp4", Duration: 2, Offset: 0, Length: 200},
		{URL: "https://cdn.example.com/media/1234/a.mp4", Duration: 2, Offset: 100, Length: 300},
		{URL: "https://cdn.example.com/media/1234/b.mp4", Duration: 2, Offset: 200, Length: 400},
	}
	for i, w := range want {
		if i >= len(p.Segments) {
			t.Fatalf("got %d segments, want %d", len(p.Segments), len(want))
		}
		if p.Segments[i] != w {
			t.Errorf("segment %d = %+v, want %+v", i, p.Segments[i], w)
		}
	}
}

func TestParseRefusesPlaylistsItCannotDownloadCorrectly(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		base    string
		wantErr error
		why     string
	}{
		{
			name:    "HTML error page served as m3u8",
			data:    "<!DOCTYPE html>\n<html><body>404</body></html>\n",
			wantErr: ErrNotPlaylist,
			why:     "saving an error page as a video must be refused",
		},
		{
			name:    "empty body",
			data:    "",
			wantErr: ErrNotPlaylist,
			why:     "there is no #EXTM3U header",
		},
		{
			name: "live stream without ENDLIST",
			data: `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:2680
#EXTINF:4.0,
fileSequence2680.ts
#EXTINF:4.0,
fileSequence2681.ts
`,
			wantErr: ErrLive,
			why:     "an endless stream has no size and never completes",
		},
		{
			name: "AES-128 encrypted segments",
			data: `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example.com/k?id=1,2",IV=0x9c7db8778570d05c3f4c1a6b7b3fc0f5
#EXTINF:10.0,
s0.ts
#EXT-X-ENDLIST
`,
			wantErr: ErrEncrypted,
			why:     "Surge would store ciphertext and produce an unplayable file",
		},
		{
			name: "SAMPLE-AES encrypted segments",
			data: `#EXTM3U
#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://key",KEYFORMAT="com.apple.streamingkeydelivery"
#EXTINF:10.0,
s0.ts
#EXT-X-ENDLIST
`,
			wantErr: ErrEncrypted,
			why:     "partially encrypted samples are just as unplayable",
		},
		{
			name: "header only",
			data: `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
`,
			wantErr: ErrEmptyPlaylist,
			why:     "no variants and no segments means nothing to download",
		},
		{
			name: "relative base URL",
			data: `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXTINF:4.0,
s0.ts
#EXT-X-ENDLIST
`,
			base:    "media/master.m3u8",
			wantErr: ErrInvalidBaseURL,
			why:     "relative segment URIs cannot be resolved without an absolute base",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == "" {
				base = baseURL
			}
			p, err := Parse([]byte(tc.data), base)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse error = %v, want %v: %s", err, tc.wantErr, tc.why)
			}
			if p != nil {
				t.Errorf("Parse returned playlist %+v alongside an error; a caller must never get a half-parsed playlist", p)
			}
		})
	}
}

func TestParseRejectsMalformedSegmentMetadata(t *testing.T) {
	tests := []struct {
		name string
		data string
		why  string
	}{
		{
			name: "EXTINF duration is not a number",
			data: "#EXTM3U\n#EXTINF:abc,\ns0.ts\n#EXT-X-ENDLIST\n",
			why:  "a bogus duration would corrupt progress and size estimates",
		},
		{
			name: "BYTERANGE length is not a number",
			data: "#EXTM3U\n#EXTINF:4.0,\n#EXT-X-BYTERANGE:x@0\ns0.ts\n#EXT-X-ENDLIST\n",
			why:  "an unparseable range would download the wrong bytes",
		},
		{
			name: "EXT-X-MAP without URI",
			data: "#EXTM3U\n#EXT-X-MAP:BYTERANGE=\"1024@0\"\n#EXTINF:4.0,\ns0.mp4\n#EXT-X-ENDLIST\n",
			why:  "an fMP4 stream without its init section is unplayable",
		},
		{
			name: "EXT-X-MAP changes mid-playlist",
			data: "#EXTM3U\n#EXT-X-MAP:URI=\"init0.mp4\"\n#EXTINF:4.0,\ns0.mp4\n#EXT-X-MAP:URI=\"init1.mp4\"\n#EXTINF:4.0,\ns1.mp4\n#EXT-X-ENDLIST\n",
			why:  "only one init section can be written, so a second one must not be dropped silently",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse([]byte(tc.data), baseURL)
			if err == nil {
				t.Fatalf("Parse succeeded with %+v, want an error: %s", p, tc.why)
			}
			if p != nil {
				t.Errorf("Parse returned playlist %+v alongside an error", p)
			}
		})
	}
}

func TestParseAcceptsRepeatedIdenticalMapTag(t *testing.T) {
	// Re-announcing the same init section after a discontinuity is legal and
	// must not fail the download.
	const playlist = `#EXTM3U
#EXT-X-MAP:URI="init.mp4",BYTERANGE="800@0"
#EXTINF:4.0,
s0.mp4
#EXT-X-DISCONTINUITY
#EXT-X-MAP:URI="init.mp4",BYTERANGE="800@0"
#EXTINF:4.0,
s1.mp4
#EXT-X-ENDLIST
`
	p, err := Parse([]byte(playlist), baseURL)
	if err != nil {
		t.Fatalf("Parse failed on a repeated identical EXT-X-MAP: %v", err)
	}
	want := Segment{URL: "https://cdn.example.com/media/1234/init.mp4", Offset: 0, Length: 800}
	if p.InitSection == nil || *p.InitSection != want {
		t.Errorf("InitSection = %+v, want %+v", p.InitSection, want)
	}
	if len(p.Segments) != 2 {
		t.Errorf("got %d segments, want 2", len(p.Segments))
	}
}

func TestEstimatedSizeGivesAProgressTargetOnlyWhenItCan(t *testing.T) {
	tests := []struct {
		name      string
		playlist  *Playlist
		bandwidth int
		want      int64
		why       string
	}{
		{
			name:      "bandwidth times duration in bytes",
			playlist:  &Playlist{TotalDuration: 60},
			bandwidth: 5200000,
			want:      39000000,
			why:       "5.2 Mbit/s for 60 s is 39 MB",
		},
		{
			name:      "fractional result is rounded",
			playlist:  &Playlist{TotalDuration: 21.75},
			bandwidth: 1400001,
			want:      3806253,
			why:       "1400001/8*21.75 = 3806252.71875",
		},
		{
			name:      "zero bandwidth cannot be estimated",
			playlist:  &Playlist{TotalDuration: 60},
			bandwidth: 0,
			want:      0,
			why:       "a variant with no BANDWIDTH attribute says nothing about size",
		},
		{
			name:      "negative bandwidth cannot be estimated",
			playlist:  &Playlist{TotalDuration: 60},
			bandwidth: -5200000,
			want:      0,
			why:       "a negative size would corrupt progress reporting",
		},
		{
			name:      "zero duration cannot be estimated",
			playlist:  &Playlist{},
			bandwidth: 5200000,
			want:      0,
			why:       "a master playlist has no segment durations",
		},
		{
			name:      "nil playlist cannot be estimated",
			playlist:  nil,
			bandwidth: 5200000,
			want:      0,
			why:       "callers may pass the result of a failed parse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimatedSize(tc.playlist, tc.bandwidth); got != tc.want {
				t.Errorf("EstimatedSize = %d, want %d: %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestEstimatedSizeUsesTheParsedTotalDuration(t *testing.T) {
	p, err := Parse([]byte(mediaPlaylist), baseURL)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	// 5.2 Mbit/s over the playlist's 21.75 s.
	const want = int64(14137500)
	if got := EstimatedSize(p, 5200000); got != want {
		t.Errorf("EstimatedSize of the parsed media playlist = %d, want %d", got, want)
	}
}

func TestParseAttributeListKeepsQuotedCommasTogether(t *testing.T) {
	got := parseAttributes(`BANDWIDTH=5200000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,NAME="A, B",EMPTY=""`)
	want := map[string]string{
		"BANDWIDTH":  "5200000",
		"CODECS":     "avc1.640028,mp4a.40.2",
		"RESOLUTION": "1920x1080",
		"NAME":       "A, B",
		"EMPTY":      "",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d attributes, want %d: %v", len(got), len(want), got)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("attribute %s = %q, want %q", name, got[name], w)
		}
	}
}
