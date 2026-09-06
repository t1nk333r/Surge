package extractor

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestLiveResolve is a manual smoke test against real sites. It is skipped
// unless SURGE_LIVE_EXTRACTOR=1 so CI never depends on the network or yt-dlp.
func TestLiveResolve(t *testing.T) {
	if os.Getenv("SURGE_LIVE_EXTRACTOR") != "1" {
		t.Skip("set SURGE_LIVE_EXTRACTOR=1 to run the live extractor smoke test")
	}
	y := NewYtDlp()
	if !y.Available() {
		t.Skip("yt-dlp not installed")
	}
	for _, url := range []string{
		"https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		"https://www.youtube.com/watch?v=BaW_jenozKc",
		"https://example.com/",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		media, err := y.Resolve(ctx, url, Options{})
		cancel()
		if err != nil {
			t.Logf("%s -> %v (ErrNoDirectFormat=%v ErrUnsupportedURL=%v)", url, err,
				errors.Is(err, ErrNoDirectFormat), errors.Is(err, ErrUnsupportedURL))
			continue
		}
		t.Logf("%s -> format=%s size=%d file=%q headers=%d url=%.70s", url, media.FormatID, media.Size, media.Filename, len(media.Headers), media.URL)
	}
}
