package orchestrator

import (
	"path/filepath"
	"testing"

	"github.com/SurgeDM/Surge/internal/store"
	"github.com/SurgeDM/Surge/internal/types"
)

// A page that resolves to one direct format is recorded under its signed
// media URL, so adding the same page again must be matched through SourceURL,
// both for a saved record and for one still in the pool.
func TestCheckForDuplicateMatchesExtractedPageURL(t *testing.T) {
	tmp := t.TempDir()
	store.Configure(filepath.Join(tmp, "surge.db"))
	t.Cleanup(func() { store.CloseDB() })

	const page = "https://site.example/watch?v=1"
	record := types.DownloadRecord{
		ID:        "extracted",
		URL:       "https://cdn.example/media.mp4?sig=abc",
		SourceURL: page,
		DestPath:  filepath.Join(tmp, "clip.mp4"),
		Filename:  "clip.mp4",
		Status:    "completed",
	}
	if err := store.AddToMasterList(record); err != nil {
		t.Fatal(err)
	}

	if got := CheckForDuplicate(page, nil); got == nil || !got.Exists {
		t.Fatalf("saved extracted download not found by its page URL: %+v", got)
	}

	store.CloseDB()
	store.Configure(filepath.Join(t.TempDir(), "surge.db"))
	active := func() map[string]*types.DownloadRecord {
		return map[string]*types.DownloadRecord{record.ID: &record}
	}
	if got := CheckForDuplicate(page+"/", active); got == nil || !got.Exists {
		t.Fatalf("active extracted download not found by its page URL: %+v", got)
	}
	if got := CheckForDuplicate("https://site.example/watch?v=2", active); got != nil {
		t.Fatalf("a different page was reported as a duplicate: %+v", got)
	}
}
