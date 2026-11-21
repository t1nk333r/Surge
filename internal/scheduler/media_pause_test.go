package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/testutil"
	"github.com/SurgeDM/Surge/internal/types"
)

// Extractor sizes are estimates. When they understate the streams, the
// supervisor publishes VerifiedProgress above the combined total; the
// scheduler's completion-boundary guard must still honour a user's pause.
func TestPartedPauseHonouredWhenSizeEstimateUnderstates(t *testing.T) {
	testutil.SetupStateDB(t)
	withMuxer(t, &recordingMuxer{available: true})
	const real = 32 << 20
	const approx = real * 9 / 10 // a 10% understatement
	srv := testutil.NewStreamingMockServerT(t, real, testutil.WithRangeSupport(true), testutil.WithLatency(5*time.Millisecond))
	defer srv.Close()

	dir := t.TempDir()
	ch := make(chan types.DownloadEvent, 256)
	pool := New(ch, 1)
	defer pool.GracefulShutdown()
	rec := partedRecord(t, dir, "clip.mkv", srv.URL()+"?v", srv.URL()+"?a", approx, approx)
	rec.TotalSize = 0
	_ = os.WriteFile(filepath.Join(dir, "clip.mkv")+types.IncompleteSuffix, nil, 0o644)
	// Slow enough to pause inside the overrun window.
	rec.RateLimit, rec.RateLimitSet = 16<<20, true
	pool.Add(*rec)
	state := progress.CfgProgress(rec)
	deadline := time.Now().Add(20 * time.Second)
	var vp, total int64
	for time.Now().Before(deadline) {
		vp = state.Bytes.VerifiedProgress.Load()
		total = state.Bytes.TotalSize.Load()
		if total > 0 && vp > total && vp < 2*real {
			break
		}
		time.Sleep(time.Millisecond)
	}
	t.Logf("at pause: verified=%d total=%d", vp, total)
	res := pool.Pause(rec.ID)
	t.Logf("Pause result found=%v paused=%v pausing=%v", res.Found, state.IsPaused(), state.IsPausing())
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case types.EventPaused:
				t.Logf("paused as requested")
				return
			case types.EventComplete:
				t.Fatalf("pause acknowledged (found=%v) but the download ran to completion", res.Found)
			}
		case <-timeout:
			t.Fatal("neither paused nor completed")
		}
	}
}

// A pause that lands while no fragment request is in flight makes the HLS
// path return ErrPaused. The scheduler treats a typed ErrPaused as "EventPaused
// already sent", which the media paths never do, so the record's pause event
// used to be lost and the master list stayed "downloading".
func TestHLSPausePublishesEventPaused(t *testing.T) {
	withMuxer(t, &recordingMuxer{available: true})
	const n = 3000
	mux := http.NewServeMux()
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-VERSION:3\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.0,\nfrag%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("0123456789")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lost := 0
	const runs = 20
	for run := 0; run < runs; run++ {
		dir := t.TempDir()
		ch := make(chan types.DownloadEvent, 256)
		pool := New(ch, 1)
		rec := manifestRecord(t, dir, "v.mp4", srv.URL+"/media.m3u8")
		rec.Runtime.Workers = 1
		_ = os.WriteFile(filepath.Join(dir, "v.mp4")+types.IncompleteSuffix, nil, 0o644)
		pool.Add(*rec)
		state := progress.CfgProgress(rec)
		deadline := time.Now().Add(5 * time.Second)
		for state.Bytes.Downloaded.Load() < 200 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		pool.Pause(rec.ID)
		gotPaused := false
		timeout := time.After(1500 * time.Millisecond)
	loop:
		for {
			select {
			case ev := <-ch:
				if ev.Type == types.EventPaused {
					gotPaused = true
					break loop
				}
			case <-timeout:
				break loop
			}
		}
		if !gotPaused {
			lost++
		}
		pool.Cancel(rec.ID)
		pool.GracefulShutdown()
	}
	t.Logf("EventPaused lost in %d/%d HLS pauses", lost, runs)
	if lost > 0 {
		t.Errorf("EventPaused lost in %d/%d HLS pauses", lost, runs)
	}
}
