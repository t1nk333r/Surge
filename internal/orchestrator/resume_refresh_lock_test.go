package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SurgeDM/Surge/internal/extractor"
	"github.com/SurgeDM/Surge/internal/scheduler"
	"github.com/SurgeDM/Surge/internal/store"
	"github.com/SurgeDM/Surge/internal/types"
)

// blockingExtractor holds a resume inside its URL refresh until released.
type blockingExtractor struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingExtractor) Name() string    { return "blocking" }
func (b *blockingExtractor) Available() bool { return true }
func (b *blockingExtractor) Resolve(ctx context.Context, _ string, _ extractor.Options) (*extractor.Media, error) {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return &extractor.Media{URL: "http://127.0.0.1:1/fresh.mp4"}, nil
}

func seedPausedExtracted(t *testing.T, id string) {
	t.Helper()
	tmp := t.TempDir()
	store.Configure(filepath.Join(tmp, "surge.db"))
	t.Cleanup(func() { store.CloseDB() })
	if err := store.AddToMasterList(types.DownloadRecord{
		ID:        id,
		URL:       "http://127.0.0.1:1/old.mp4",
		DestPath:  filepath.Join(tmp, "clip.mp4"),
		Filename:  "clip.mp4",
		Status:    "paused",
		SourceURL: "https://site.example/watch?v=1",
	}); err != nil {
		t.Fatal(err)
	}
}

// A resume re-resolves an extracted URL before admission, and in that window
// the ID is in neither the queue nor the active set. A Cancel or Pause issued
// then must wait for the resume and act on the admitted download, not find
// nothing, succeed, and let the download start.
func TestLifecycleOrderedAgainstResumeRefresh(t *testing.T) {
	for _, op := range []string{"cancel", "pause"} {
		t.Run(op, func(t *testing.T) {
			id := "refresh-" + op
			seedPausedExtracted(t, id)
			pool := scheduler.New(nil, 1)
			mgr := NewLifecycleManager(pool, nil, nil)
			defer mgr.Shutdown()
			ex := &blockingExtractor{entered: make(chan struct{}), release: make(chan struct{})}
			mgr.SetMediaExtractor(ex)

			resumed := make(chan error, 1)
			go func() { resumed <- mgr.Resume(id) }()
			<-ex.entered

			acted := make(chan error, 1)
			go func() {
				if op == "cancel" {
					acted <- mgr.Cancel(id)
				} else {
					acted <- mgr.Pause(id)
				}
			}()
			select {
			case err := <-acted:
				t.Fatalf("%s returned (%v) while the resume was still re-resolving", op, err)
			case <-time.After(50 * time.Millisecond):
			}
			close(ex.release)
			if err := <-resumed; err != nil {
				t.Fatalf("resume: %v", err)
			}
			if err := <-acted; err != nil {
				t.Fatalf("%s: %v", op, err)
			}

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				st := pool.GetStatus(id)
				if st == nil || st.Status == "paused" {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			st := pool.GetStatus(id)
			pool.Cancel(id)
			t.Fatalf("%s during the resume refresh was lost: status=%q", op, st.Status)
		})
	}
}
