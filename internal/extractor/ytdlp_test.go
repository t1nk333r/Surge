package extractor

import (
	"errors"
	"testing"
)

// A video whose only https formats are video-only or audio-only, plus HLS
// variants — the shape YouTube actually returns for a modern 4K upload. It has
// no single-file format, so it must be refused rather than downloaded silent.
const splitOnlyJSON = `{
  "_type": "video",
  "title": "Big Buck Bunny 60fps 4K",
  "ext": "webm",
  "formats": [
    {"format_id": "401", "url": "https://cdn/av01", "ext": "mp4", "protocol": "https",
     "acodec": "none", "vcodec": "av01.0.13M.08", "filesize": 712445280, "height": 2160, "tbr": 9000,
     "http_headers": {"User-Agent": "Mozilla/5.0"}},
    {"format_id": "251", "url": "https://cdn/opus", "ext": "webm", "protocol": "https",
     "acodec": "opus", "vcodec": "none", "filesize": 3871021, "tbr": 130,
     "http_headers": {"User-Agent": "Mozilla/5.0"}},
    {"format_id": "hls-720", "url": "https://cdn/master.m3u8", "ext": "mp4", "protocol": "m3u8_native",
     "acodec": "mp4a.40.2", "vcodec": "avc1.4d401f", "height": 720, "tbr": 2000}
  ]
}`

// A video that still offers progressive formats: 360p and 720p muxed streams
// alongside split and fragmented ones.
const progressiveJSON = `{
  "_type": "video",
  "title": "Example: Clip / Part 1?",
  "ext": "mp4",
  "http_headers": {"User-Agent": "Mozilla/5.0", "Accept-Language": "en-us"},
  "formats": [
    {"format_id": "18", "url": "https://cdn/360p", "ext": "mp4", "protocol": "https",
     "acodec": "mp4a.40.2", "vcodec": "avc1.42001E", "filesize": 10485760, "height": 360, "tbr": 600},
    {"format_id": "22", "url": "https://cdn/720p", "ext": "mp4", "protocol": "https",
     "acodec": "mp4a.40.2", "vcodec": "avc1.64001F", "filesize_approx": 52428800, "height": 720, "tbr": 1500,
     "http_headers": {"Cookie": "consent=1"}},
    {"format_id": "137", "url": "https://cdn/1080p-video-only", "ext": "mp4", "protocol": "https",
     "acodec": "none", "vcodec": "avc1.640028", "filesize": 104857600, "height": 1080, "tbr": 4000},
    {"format_id": "hls-1080", "url": "https://cdn/master.m3u8", "ext": "mp4", "protocol": "m3u8_native",
     "acodec": "mp4a.40.2", "vcodec": "avc1.640028", "height": 1080, "tbr": 4500}
  ]
}`

// Audio-only media (a podcast episode): no video anywhere, so the best audio
// stream is a complete download.
const audioOnlyJSON = `{
  "_type": "video",
  "title": "Episode 42",
  "ext": "mp3",
  "formats": [
    {"format_id": "low", "url": "https://cdn/low.mp3", "ext": "mp3", "protocol": "https",
     "acodec": "mp3", "vcodec": "none", "filesize": 1048576, "tbr": 64},
    {"format_id": "high", "url": "https://cdn/high.mp3", "ext": "mp3", "protocol": "https",
     "acodec": "mp3", "vcodec": "none", "filesize": 4194304, "tbr": 192}
  ]
}`

// Extractors for a single media file report the URL at the top level.
const bareURLJSON = `{
  "_type": "video",
  "title": "A Picture",
  "ext": "jpg",
  "url": "https://cdn/picture.jpg",
  "protocol": "https",
  "filesize": 20480,
  "http_headers": {"User-Agent": "Mozilla/5.0"},
  "formats": []
}`

const playlistJSON = `{
  "_type": "playlist",
  "title": "My List",
  "entries": [{"title": "one"}, {"title": "two"}]
}`

func TestResolvePicksBestProgressiveFormat(t *testing.T) {
	media, err := parseYtDlpJSON([]byte(progressiveJSON), "https://example.com/watch?v=1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if media.FormatID != "22" {
		t.Errorf("format = %q, want the 720p progressive stream (22)", media.FormatID)
	}
	if media.URL != "https://cdn/720p" {
		t.Errorf("url = %q", media.URL)
	}
	// filesize_approx must be used when filesize is absent, so the engine can
	// size the file and pick a chunk layout.
	if media.Size != 52428800 {
		t.Errorf("size = %d, want the approximate size 52428800", media.Size)
	}
	if media.SourceURL != "https://example.com/watch?v=1" {
		t.Errorf("source url = %q", media.SourceURL)
	}
	// Format headers must win over the top-level ones, and both must survive.
	if got := media.Headers["Cookie"]; got != "consent=1" {
		t.Errorf("format headers lost: %v", media.Headers)
	}
	if got := media.Headers["User-Agent"]; got != "Mozilla/5.0" {
		t.Errorf("top-level headers lost: %v", media.Headers)
	}
	// Path separators and Windows-illegal characters must not reach the FS.
	if media.Filename != "Example_ Clip _ Part 1_.mp4" {
		t.Errorf("filename = %q", media.Filename)
	}
}

// Modern video sites usually publish video and audio separately. Those are
// downloadable, just not as one request, so they come back as a pair of parts.
func TestResolveFallsBackToStreamPair(t *testing.T) {
	media, err := parseYtDlpJSON([]byte(splitOnlyJSON), "https://example.com/watch?v=2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(media.Parts) != 2 {
		t.Fatalf("parts = %d, want video + audio", len(media.Parts))
	}
	video, audio := media.Parts[0], media.Parts[1]
	if video.Kind != KindVideo || video.FormatID != "401" {
		t.Errorf("video part = %+v", video)
	}
	if audio.Kind != KindAudio || audio.FormatID != "251" {
		t.Errorf("audio part = %+v", audio)
	}
	// The fragmented HLS variant carries both codecs but is not one file, so
	// it must not be chosen over the pair.
	if video.URL == "https://cdn/master.m3u8" || audio.URL == "https://cdn/master.m3u8" {
		t.Error("a fragmented format leaked into the parts")
	}
	if media.Size != 712445280+3871021 {
		t.Errorf("size = %d, want the sum of both parts", media.Size)
	}
	// A parted item has no single media URL; the page URL is its identity.
	if media.URL != "https://example.com/watch?v=2" {
		t.Errorf("url = %q, want the page URL", media.URL)
	}
	// mp4 video plus webm audio can only go in mkv.
	if media.Filename != "Big Buck Bunny 60fps 4K.mkv" {
		t.Errorf("filename = %q", media.Filename)
	}
	if media.FormatID != "401+251" {
		t.Errorf("format = %q", media.FormatID)
	}
	if video.Headers["User-Agent"] != "Mozilla/5.0" {
		t.Errorf("part headers lost: %v", video.Headers)
	}
}

// When a site offers nothing but fragmented streams, the best playlist that
// carries both audio and video becomes the download: the engine assembles it.
func TestResolveFallsBackToFragmentedStream(t *testing.T) {
	const fragmentedOnly = `{
	  "_type": "video",
	  "title": "Fragments Only",
	  "formats": [
	    {"format_id": "hls-480", "url": "https://cdn/480.m3u8", "ext": "mp4", "protocol": "m3u8_native",
	     "acodec": "mp4a.40.2", "vcodec": "avc1.4d401f", "height": 480},
	    {"format_id": "hls-720", "url": "https://cdn/720.m3u8", "ext": "mp4", "protocol": "m3u8_native",
	     "acodec": "mp4a.40.2", "vcodec": "avc1.4d401f", "height": 720},
	    {"format_id": "hls-audio", "url": "https://cdn/audio.m3u8", "ext": "mp4", "protocol": "m3u8_native",
	     "acodec": "mp4a.40.2", "vcodec": "none"}
	  ]
	}`

	media, err := parseYtDlpJSON([]byte(fragmentedOnly), "https://example.com/live")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if media.ManifestURL != "https://cdn/720.m3u8" {
		t.Errorf("manifest = %q, want the best muxed playlist", media.ManifestURL)
	}
	// A fragmented item has no single media URL, and no parts either.
	if media.URL != "https://example.com/live" || len(media.Parts) != 0 {
		t.Errorf("url = %q parts = %d", media.URL, len(media.Parts))
	}
	if media.Filename != "Fragments Only.mp4" {
		t.Errorf("filename = %q", media.Filename)
	}
}

// A stream with no audio anywhere — file, pair or playlist — cannot produce a
// complete download and must be refused rather than saved silent.
func TestResolveRefusesMediaWithNoAudioAtAll(t *testing.T) {
	const videoWithoutAudio = `{
	  "_type": "video",
	  "title": "Silent Clip",
	  "formats": [
	    {"format_id": "137", "url": "https://cdn/1080p", "ext": "mp4", "protocol": "https",
	     "acodec": "none", "vcodec": "avc1.640028", "height": 1080},
	    {"format_id": "hls-video", "url": "https://cdn/video.m3u8", "ext": "mp4", "protocol": "m3u8_native",
	     "acodec": "none", "vcodec": "avc1.640028", "height": 1080}
	  ]
	}`

	if _, err := parseYtDlpJSON([]byte(videoWithoutAudio), "https://example.com/silent"); !errors.Is(err, ErrNoDirectFormat) {
		t.Fatalf("err = %v, want ErrNoDirectFormat", err)
	}
}

// The proxy is Surge's, not the environment's: yt-dlp runs outside the shared
// transport pool, so a proxied Surge must hand it the proxy explicitly or the
// page is resolved over a direct connection.
func TestYtDlpArgsCarryTheConfiguredProxy(t *testing.T) {
	args := ytDlpArgs("https://example.com/watch?v=1", Options{ProxyURL: "socks5://127.0.0.1:9050"})

	proxyAt := -1
	for i, arg := range args {
		if arg == "--proxy" {
			proxyAt = i
			break
		}
	}
	if proxyAt < 0 || proxyAt+1 >= len(args) {
		t.Fatalf("args = %v, want a --proxy flag", args)
	}
	if args[proxyAt+1] != "socks5://127.0.0.1:9050" {
		t.Errorf("proxy value = %q", args[proxyAt+1])
	}
	// The URL must stay a positional operand behind the terminator, whatever
	// else is added to the argument list.
	if args[len(args)-2] != "--" || args[len(args)-1] != "https://example.com/watch?v=1" {
		t.Errorf("args tail = %v, want [-- <url>]", args[len(args)-2:])
	}
}

func TestYtDlpArgsOmitProxyWhenUnset(t *testing.T) {
	// No proxy configured must mean "inherit the environment", matching the
	// transport pool's http.ProxyFromEnvironment default, not "--proxy ''",
	// which yt-dlp reads as "never use a proxy".
	for _, opts := range []Options{{}, {ProxyURL: "   "}} {
		for _, arg := range ytDlpArgs("https://example.com/x", opts) {
			if arg == "--proxy" {
				t.Fatalf("opts %+v produced a --proxy flag", opts)
			}
		}
	}
}

func TestResolveAcceptsBestAudioWhenThereIsNoVideo(t *testing.T) {
	media, err := parseYtDlpJSON([]byte(audioOnlyJSON), "https://example.com/ep/42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if media.FormatID != "high" {
		t.Errorf("format = %q, want the 192kbps stream", media.FormatID)
	}
	if media.Filename != "Episode 42.mp3" {
		t.Errorf("filename = %q", media.Filename)
	}
}

func TestResolveUsesTopLevelURLWhenNoFormatsAreListed(t *testing.T) {
	media, err := parseYtDlpJSON([]byte(bareURLJSON), "https://example.com/pic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if media.URL != "https://cdn/picture.jpg" || media.Size != 20480 {
		t.Errorf("media = %+v", media)
	}
}

func TestResolveRefusesPlaylists(t *testing.T) {
	_, err := parseYtDlpJSON([]byte(playlistJSON), "https://example.com/list")
	if !errors.Is(err, ErrPlaylist) {
		t.Fatalf("err = %v, want ErrPlaylist", err)
	}
}

func TestLooksExtractableOnlyForPages(t *testing.T) {
	cases := map[string]bool{
		"text/html":                true,
		"text/html; charset=utf-8": true,
		"application/xhtml+xml":    true,
		"video/mp4":                false,
		"application/octet-stream": false,
		"":                         false,
	}
	for contentType, want := range cases {
		if got := LooksExtractable(contentType); got != want {
			t.Errorf("LooksExtractable(%q) = %v, want %v", contentType, got, want)
		}
	}
}

func TestUnavailableExtractorReportsItself(t *testing.T) {
	y := &YtDlp{Binary: "surge-nonexistent-extractor-binary"}
	if y.Available() {
		t.Fatal("a missing binary must not report itself as available")
	}
	if _, err := y.Resolve(nil, "https://example.com", Options{}); !errors.Is(err, ErrNotAvailable) { //nolint:staticcheck // nil ctx is never reached
		t.Fatalf("err = %v, want ErrNotAvailable", err)
	}
}
