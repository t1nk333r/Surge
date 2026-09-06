package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/SurgeDM/Surge/internal/config"
	"github.com/SurgeDM/Surge/internal/extractor"
	"github.com/SurgeDM/Surge/internal/store"
	"github.com/SurgeDM/Surge/internal/types"
)

type fakeExtractor struct {
	available bool
	media     *extractor.Media
	err       error
	calls     int
	gotURL    string
	gotOpts   extractor.Options
}

func (f *fakeExtractor) Name() string    { return "fake" }
func (f *fakeExtractor) Available() bool { return f.available }

func (f *fakeExtractor) Resolve(_ context.Context, pageURL string, opts extractor.Options) (*extractor.Media, error) {
	f.calls++
	f.gotURL = pageURL
	f.gotOpts = opts
	return f.media, f.err
}

func TestExtractMediaRewritesRequest(t *testing.T) {
	ex := &fakeExtractor{
		available: true,
		media: &extractor.Media{
			URL:       "https://cdn.example/video.mp4?sig=abc",
			Headers:   map[string]string{"User-Agent": "yt-dlp/1", "Cookie": "b=2"},
			Filename:  "Clip.mp4",
			Size:      1024,
			FormatID:  "22",
			SourceURL: "https://site.example/watch?v=1",
		},
	}
	mgr := &LifecycleManager{mediaExtractor: ex}

	req := &DownloadRequest{
		URL:     "https://site.example/watch?v=1",
		Headers: map[string]string{"Cookie": "a=1", "Referer": "https://site.example/"},
		Mirrors: []string{"https://site.example/watch?v=1&mirror=2"},
	}

	extracted, err := mgr.extractMedia(context.Background(), req, mediaExtractionTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !extracted {
		t.Fatal("expected the request to be rewritten")
	}

	if req.URL != "https://cdn.example/video.mp4?sig=abc" {
		t.Errorf("url = %q", req.URL)
	}
	if req.SourceURL != "https://site.example/watch?v=1" {
		t.Errorf("source url = %q, resume needs the page URL", req.SourceURL)
	}
	if req.FormatID != "22" {
		t.Errorf("format = %q", req.FormatID)
	}
	if req.Filename != "Clip.mp4" {
		t.Errorf("filename = %q", req.Filename)
	}
	// Resolved headers are part of the URL's credential and must win, while
	// unrelated caller headers survive.
	if req.Headers["Cookie"] != "b=2" {
		t.Errorf("resolved Cookie lost: %v", req.Headers)
	}
	if req.Headers["Referer"] != "https://site.example/" {
		t.Errorf("caller Referer lost: %v", req.Headers)
	}
	// Mirrors of the page cannot serve the media file.
	if req.Mirrors != nil {
		t.Errorf("mirrors = %v, want nil", req.Mirrors)
	}
}

// Surge's proxy applies to every in-process request through the transport
// pool; the extractor is a child process, so the setting has to be handed to
// it or a proxied Surge resolves media pages over a direct connection.
func TestExtractMediaForwardsTheConfiguredProxy(t *testing.T) {
	settings := config.DefaultSettings()
	settings.Network.ProxyURL.Value = "http://127.0.0.1:8080"

	ex := &fakeExtractor{available: true, media: &extractor.Media{URL: "https://cdn/x.mp4"}}
	mgr := &LifecycleManager{mediaExtractor: ex, settings: settings}

	if _, err := mgr.extractMedia(context.Background(), &DownloadRequest{URL: "https://site/watch"}, mediaExtractionTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ex.gotOpts.ProxyURL != "http://127.0.0.1:8080" {
		t.Errorf("extractor saw proxy %q, want the configured one", ex.gotOpts.ProxyURL)
	}
}

func TestExtractMediaKeepsCallerFilename(t *testing.T) {
	ex := &fakeExtractor{available: true, media: &extractor.Media{URL: "https://cdn/x.mp4", Filename: "Extracted.mp4"}}
	mgr := &LifecycleManager{mediaExtractor: ex}
	req := &DownloadRequest{URL: "https://site/watch", Filename: "chosen.mp4"}

	if _, err := mgr.extractMedia(context.Background(), req, mediaExtractionTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Filename != "chosen.mp4" {
		t.Errorf("filename = %q, an explicit request name must win", req.Filename)
	}
}

// A page the extractor recognises but cannot express as one file must fail the
// enqueue: downloading the HTML page or a silent video instead would be a
// wrong success.
func TestExtractMediaRefusesUndownloadableMedia(t *testing.T) {
	for name, sentinel := range map[string]error{
		"playlist":  extractor.ErrPlaylist,
		"no direct": extractor.ErrNoDirectFormat,
	} {
		t.Run(name, func(t *testing.T) {
			mgr := &LifecycleManager{mediaExtractor: &fakeExtractor{available: true, err: sentinel}}
			req := &DownloadRequest{URL: "https://site/watch"}

			extracted, err := mgr.extractMedia(context.Background(), req, mediaExtractionTimeout)
			if extracted {
				t.Fatal("request must not be rewritten")
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v", err, sentinel)
			}
			if req.URL != "https://site/watch" {
				t.Errorf("url must be left alone, got %q", req.URL)
			}
		})
	}
}

// An unrecognised page keeps the pre-extraction behaviour, so downloading an
// ordinary HTML file still works.
func TestExtractMediaFallsThroughForUnsupportedPages(t *testing.T) {
	mgr := &LifecycleManager{mediaExtractor: &fakeExtractor{available: true, err: extractor.ErrUnsupportedURL}}
	req := &DownloadRequest{URL: "https://blog.example/post.html"}

	extracted, err := mgr.extractMedia(context.Background(), req, mediaExtractionTimeout)
	if err != nil || extracted {
		t.Fatalf("extracted = %v, err = %v; want (false, nil)", extracted, err)
	}
}

func TestExtractMediaSkippedWhenToolMissing(t *testing.T) {
	ex := &fakeExtractor{available: false, media: &extractor.Media{URL: "https://cdn/x.mp4"}}
	mgr := &LifecycleManager{mediaExtractor: ex}
	req := &DownloadRequest{URL: "https://site/watch"}

	extracted, err := mgr.extractMedia(context.Background(), req, mediaExtractionTimeout)
	if err != nil || extracted {
		t.Fatalf("extracted = %v, err = %v; want (false, nil)", extracted, err)
	}
	if ex.calls != 0 {
		t.Errorf("resolve was called %d times on an unavailable extractor", ex.calls)
	}
}

func TestExtractMediaDisabledWithNilExtractor(t *testing.T) {
	mgr := &LifecycleManager{}
	req := &DownloadRequest{URL: "https://site/watch"}

	extracted, err := mgr.extractMedia(context.Background(), req, mediaExtractionTimeout)
	if err != nil || extracted {
		t.Fatalf("extracted = %v, err = %v; want (false, nil)", extracted, err)
	}
}

// A media URL is signed and short-lived, so resuming must re-resolve it from
// the page URL — and persist the new URL, because saved chunk state is looked
// up by (URL, DestPath).
func TestRefreshExtractedURLRewritesAndPersists(t *testing.T) {
	tmpDir := t.TempDir()
	store.Configure(filepath.Join(tmpDir, "surge.db"))
	t.Cleanup(func() { store.CloseDB() })

	entry := types.DownloadRecord{
		ID:        "id-extracted",
		URL:       "https://cdn.example/old.mp4?sig=expired",
		DestPath:  filepath.Join(tmpDir, "clip.mp4"),
		Status:    "paused",
		SourceURL: "https://site.example/watch?v=1",
		FormatID:  "22",
	}
	if err := store.AddToMasterList(entry); err != nil {
		t.Fatalf("seed master list: %v", err)
	}

	ex := &fakeExtractor{
		available: true,
		media: &extractor.Media{
			URL:      "https://cdn.example/fresh.mp4?sig=valid",
			Headers:  map[string]string{"User-Agent": "yt-dlp/1"},
			FormatID: "22",
		},
	}
	mgr := &LifecycleManager{mediaExtractor: ex}

	cfg := entry
	mgr.refreshExtractedURL(&cfg)

	if ex.gotURL != "https://site.example/watch?v=1" {
		t.Errorf("resolved %q, must re-resolve the page URL", ex.gotURL)
	}
	if cfg.URL != "https://cdn.example/fresh.mp4?sig=valid" {
		t.Errorf("cfg url = %q", cfg.URL)
	}
	if cfg.Headers["User-Agent"] != "yt-dlp/1" {
		t.Errorf("fresh headers not applied: %v", cfg.Headers)
	}

	stored, err := store.GetDownload("id-extracted")
	if err != nil || stored == nil {
		t.Fatalf("load stored record: %v", err)
	}
	if stored.URL != "https://cdn.example/fresh.mp4?sig=valid" {
		t.Errorf("stored url = %q; resume state is keyed by URL and must be updated", stored.URL)
	}
}

// Without provenance there is nothing to re-resolve, and a download that was
// never extracted must not pay for an extractor run.
func TestRefreshExtractedURLSkipsPlainDownloads(t *testing.T) {
	ex := &fakeExtractor{available: true, media: &extractor.Media{URL: "https://cdn/other.mp4"}}
	mgr := &LifecycleManager{mediaExtractor: ex}

	cfg := types.DownloadRecord{ID: "plain", URL: "https://example.com/file.iso"}
	mgr.refreshExtractedURL(&cfg)

	if ex.calls != 0 {
		t.Errorf("extractor called %d times for a plain download", ex.calls)
	}
	if cfg.URL != "https://example.com/file.iso" {
		t.Errorf("url = %q, must be untouched", cfg.URL)
	}
}

// The probe semaphore is a pre-filled token pool: a slot is taken by
// receiving. Sending instead looks like a working acquire but blocks on an
// idle daemon - where every token is home and the channel is full - so the
// resume-time re-resolve silently never ran and every extracted download
// resumed into its expired URL.
func TestRefreshExtractedURLTakesAProbeSlotOnAnIdleDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	store.Configure(filepath.Join(tmpDir, "surge.db"))
	t.Cleanup(func() { store.CloseDB() })

	entry := types.DownloadRecord{
		ID:        "id-idle",
		URL:       "https://cdn.example/old.mp4?sig=expired",
		DestPath:  filepath.Join(tmpDir, "clip.mp4"),
		Status:    "paused",
		SourceURL: "https://site.example/watch?v=1",
	}
	if err := store.AddToMasterList(entry); err != nil {
		t.Fatalf("seed master list: %v", err)
	}

	ex := &fakeExtractor{
		available: true,
		media:     &extractor.Media{URL: "https://cdn.example/fresh.mp4?sig=valid"},
	}

	// Exactly how NewLifecycleManager builds it.
	sem := make(chan struct{}, defaultMaxConcurrentProbes)
	for range defaultMaxConcurrentProbes {
		sem <- struct{}{}
	}
	mgr := &LifecycleManager{mediaExtractor: ex, probeSem: sem}

	cfg := entry
	mgr.refreshExtractedURL(&cfg)

	if ex.calls != 1 {
		t.Fatalf("extractor called %d times on an idle daemon, want 1", ex.calls)
	}
	if cfg.URL != "https://cdn.example/fresh.mp4?sig=valid" {
		t.Errorf("URL = %q, want the refreshed one", cfg.URL)
	}
	if len(sem) != defaultMaxConcurrentProbes {
		t.Errorf("probe pool holds %d tokens after the refresh, want %d",
			len(sem), defaultMaxConcurrentProbes)
	}
}
