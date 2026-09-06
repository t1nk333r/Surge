package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SurgeDM/Surge/internal/mux"
	"github.com/SurgeDM/Surge/internal/probe"
	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/store"
	"github.com/SurgeDM/Surge/internal/strategy/concurrent"
	"github.com/SurgeDM/Surge/internal/strategy/single"
	"github.com/SurgeDM/Surge/internal/types"
	"github.com/SurgeDM/Surge/internal/utils"
)

// defaultMuxer is a package variable so tests can substitute a muxer that does
// not need ffmpeg.
var defaultMuxer mux.Muxer = mux.NewFFmpeg()

// partProgressInterval is how often the combined view of a multi-part download
// is refreshed. The progress aggregator publishes every 150ms, so anything
// finer would only add contention.
const partProgressInterval = 120 * time.Millisecond

// streamSpec describes one HTTP file to acquire. It is the narrow contract
// between RunDownload and the two existing downloaders.
type streamSpec struct {
	url      string
	headers  map[string]string
	destPath string
	filename string
	// totalSize is the expected size, or 0 to let the downloader discover it.
	totalSize     int64
	supportsRange bool
	mirrors       []string
	// id is what the downloader persists its pause snapshot under, and
	// progressCh is where it publishes lifecycle events. A part MUST NOT
	// borrow the record's: the downloaders save their own URL, dest path and
	// size under the id they are given, which would rewrite the record to
	// point at "<name>.p0.video" and an expiring stream URL.
	id         string
	progressCh chan<- types.DownloadEvent
}

// acquireStream downloads one HTTP file into spec.destPath + ".surge" using the
// concurrent downloader when the server supports ranges, falling back to the
// single-stream one exactly as a plain download does. It returns the size the
// downloader ended up with.
func acquireStream(ctx context.Context, cfg *types.DownloadRecord, progState *progress.DownloadProgress, spec streamSpec) (int64, error) {
	totalSize := spec.totalSize
	useConcurrent := spec.supportsRange
	var downloadErr error

	if useConcurrent {
		utils.Debug("Using concurrent downloader for %s", spec.filename)

		// Probe every candidate mirror so a dead one cannot poison the worker
		// pool. Only plain downloads carry mirrors; parts never do.
		var activeMirrors []string
		if len(spec.mirrors) > 0 {
			allToCheck := append([]string{spec.url}, spec.mirrors...)
			runCfg := &types.RuntimeConfig{
				ProxyURL:  cfg.Runtime.ProxyURL,
				CustomDNS: cfg.Runtime.CustomDNS,
			}
			valid, errs := probe.ProbeMirrorsWithProxy(ctx, allToCheck, runCfg)
			for u, e := range errs {
				utils.Debug("Mirror probe failed for %s: %v", u, e)
			}
			for _, v := range valid {
				if v != spec.url {
					activeMirrors = append(activeMirrors, v)
				}
			}
			utils.Debug("Found %d active mirrors from %d candidates", len(activeMirrors), len(spec.mirrors))
		}

		d := concurrent.NewConcurrentDownloader(spec.id, spec.progressCh, progState, cfg.Runtime)
		d.Headers = spec.headers
		d.Limiter = cfg.Limiter
		d.RateLimitBps = cfg.RateLimit
		d.RateLimitSet = cfg.RateLimitSet
		downloadErr = d.Download(ctx, spec.url, spec.mirrors, activeMirrors, spec.destPath, totalSize)
		if d.TotalSize > 0 {
			totalSize = d.TotalSize
		}

		var downloaded int64
		if progState != nil {
			downloaded = progState.Bytes.Downloaded.Load()
		}

		// Fall back only when nothing landed: a partial concurrent transfer
		// must not be discarded.
		if shouldFallbackToSingle(downloadErr, downloaded) {
			utils.Debug("Concurrent download failed: %v - falling back to single-threaded", downloadErr)
			useConcurrent = false
			if progState != nil {
				progState.SessionReset()
			}
			_ = os.Truncate(spec.destPath+types.IncompleteSuffix, 0)
		}
	}

	if !useConcurrent {
		utils.Debug("Using single-threaded downloader for %s", spec.filename)
		d := single.NewSingleDownloader(spec.id, spec.progressCh, progState, cfg.Runtime)
		d.Headers = spec.headers
		d.Limiter = cfg.Limiter
		downloadErr = d.Download(ctx, spec.url, spec.destPath, totalSize, spec.filename)
		if d.TotalSize > 0 {
			totalSize = d.TotalSize
		}
	}

	return totalSize, downloadErr
}

// (part paths come from types.PartWorkingPath so the store's integrity sweep
// recognises them)

// runPartedDownload downloads every stream of a multi-part item and muxes them
// into the working file the orchestrator reserved, so the normal completion
// path (rename ".surge" to the final name) still applies.
//
// Each part gets its own progress state because the downloaders store absolute
// byte counts: sharing one state would make the second part's progress jump
// back to zero. A supervisor goroutine folds the parts into the record's own
// state, which is what the aggregator, the TUI and SSE read.
func runPartedDownload(ctx context.Context, cfg *types.DownloadRecord, progState *progress.DownloadProgress, finalDestPath string) (int64, error) {
	if defaultMuxer == nil || !defaultMuxer.Available() {
		return 0, fmt.Errorf("%w: cannot combine the video and audio streams", mux.ErrNotAvailable)
	}

	// The extractor may not know a stream's size. Summing what is known would
	// publish a total that is too small, and the bar would run past 100% for
	// the whole of the unmeasured stream; an unknown total lets the supervisor
	// report the sizes the responses reveal instead.
	var expectedTotal int64
	for _, part := range cfg.Parts {
		if part.Size <= 0 {
			expectedTotal = 0
			break
		}
		expectedTotal += part.Size
	}
	if progState != nil && expectedTotal > 0 {
		progState.SetTotalSize(expectedTotal)
	}

	inputs := make([]mux.Input, 0, len(cfg.Parts))
	var completedBytes int64

	for i, part := range cfg.Parts {
		if part.URL == "" {
			return 0, fmt.Errorf("part %d (%s) has no URL; the media URL may have expired", i, part.Kind)
		}

		partDest := types.PartWorkingPath(finalDestPath, i, part.Kind)
		workingPath := partDest + types.IncompleteSuffix
		inputs = append(inputs, mux.Input{Path: workingPath, Kind: muxKind(part.Kind)})

		// A recorded stream still has to be on disk: the integrity sweep, a
		// half-finished move or the user can remove a working file, and
		// muxing an empty input would produce a file with a missing track
		// rather than an error.
		onDisk := int64(-1)
		if info, statErr := os.Stat(workingPath); statErr == nil {
			onDisk = info.Size()
		}
		if part.Complete && onDisk > 0 && (part.Size <= 0 || onDisk >= part.Size) {
			size := part.Size
			if size <= 0 {
				size = onDisk
			}
			utils.Debug("Part %d (%s) already downloaded, skipping", i, part.Kind)
			completedBytes += size
			if progState != nil {
				progState.Bytes.Downloaded.Store(completedBytes)
				progState.Bytes.VerifiedProgress.Store(completedBytes)
			}
			continue
		}
		if part.Complete {
			utils.Debug("Part %d (%s) was recorded complete but holds %d of %d bytes; downloading it again",
				i, part.Kind, onDisk, part.Size)
		}

		// The downloaders require the working file to exist already; for a
		// plain download the orchestrator reserves it, for parts we do.
		if err := ensureWorkingFile(workingPath); err != nil {
			return 0, err
		}

		partState := progress.New(fmt.Sprintf("%s#%s", cfg.ID, part.Kind), part.Size)
		partState.SetDestPath(partDest)
		partState.SetFilename(filepath.Base(partDest))
		partState.SetRateLimit(cfg.RateLimit, cfg.RateLimitSet)
		partState.SyncSessionStart()

		stopSupervisor := supervisePart(progState, partState, completedBytes, expectedTotal)
		partID := fmt.Sprintf("%s#p%d", cfg.ID, i)
		size, err := acquireStream(ctx, cfg, partState, streamSpec{
			url:      part.URL,
			headers:  part.Headers,
			destPath: partDest,
			filename: filepath.Base(partDest),
			// Part sizes come from the extractor and are sometimes
			// approximate, so let the downloader confirm from the response.
			totalSize:     0,
			supportsRange: true,
			// The part's own store id: its snapshot describes one stream, not
			// the download, and writing it under the record's id would rewrite
			// the record to point at the part file and an expiring stream URL.
			// No event channel either - the record's lifecycle events are
			// published by the scheduler around this call.
			id: partID,
		})
		stopSupervisor()

		// A part's snapshot is never read: a resumed part restarts from zero
		// (resume is at stream granularity), so the file would just accumulate.
		if delErr := store.DeleteState(partID); delErr != nil && !errors.Is(delErr, types.ErrNotFound) {
			utils.Debug("Could not drop part state %s: %v", partID, delErr)
		}

		if err != nil {
			return 0, err
		}

		if size <= 0 {
			size = partState.Bytes.Downloaded.Load()
		}

		// Record the finished stream before starting the next one: a pause or
		// crash in the second stream must not cost the first one.
		cfg.Parts[i].Complete = true
		if err := store.SetPartComplete(cfg.ID, i); err != nil {
			utils.Debug("Could not record part %d of %s as complete: %v", i, cfg.ID, err)
		}

		completedBytes += size
		if progState != nil {
			progState.Bytes.Downloaded.Store(completedBytes)
			progState.Bytes.VerifiedProgress.Store(completedBytes)
		}
	}

	// ffmpeg picks its output muxer from the file extension, and the reserved
	// working file ends in ".surge", which means nothing to it. Mux into a
	// sibling that keeps the container extension and move it into place: a
	// rename in the same directory is atomic, so the working file is either
	// untouched or complete.
	target := finalDestPath + types.IncompleteSuffix
	muxTarget := types.MuxWorkingPath(finalDestPath)
	utils.Debug("Muxing %d parts into %s with %s", len(inputs), muxTarget, defaultMuxer.Name())
	if err := defaultMuxer.Mux(ctx, muxTarget, inputs...); err != nil {
		return 0, fmt.Errorf("combining the downloaded streams failed: %w", err)
	}
	if err := os.Rename(muxTarget, target); err != nil {
		_ = os.Remove(muxTarget)
		return 0, fmt.Errorf("could not move the combined file into place: %w", err)
	}
	// The muxer writes through a private temp file (0600); plain downloads
	// leave 0644, so keep the two paths indistinguishable to the user.
	if err := os.Chmod(target, 0o644); err != nil {
		utils.Debug("Could not normalise permissions on %s: %v", target, err)
	}

	for _, input := range inputs {
		if err := os.Remove(input.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			utils.Debug("Could not remove part file %s: %v", input.Path, err)
		}
	}

	finalSize := completedBytes
	if info, err := os.Stat(target); err == nil {
		// The muxed container is a different size from the sum of its inputs.
		finalSize = info.Size()
	}
	if progState != nil {
		progState.SetTotalSize(finalSize)
		progState.Bytes.Downloaded.Store(finalSize)
		progState.Bytes.VerifiedProgress.Store(finalSize)
	}
	return finalSize, nil
}

// supervisePart mirrors a pause request down into the running part and folds
// the part's byte counter into the record's combined view. The returned
// function stops it.
func supervisePart(combined, part *progress.DownloadProgress, baseBytes, expectedTotal int64) func() {
	if combined == nil || part == nil {
		return func() {}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(partProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Pause is requested on the record's state; the part's
				// downloader only watches its own, so relay it.
				if combined.IsPausing() || combined.IsPaused() {
					part.Pause()
				}
				downloaded, partTotal, _, _, connections, _ := part.GetProgress()
				combined.Bytes.Downloaded.Store(baseBytes + downloaded)
				combined.Bytes.VerifiedProgress.Store(baseBytes + part.Bytes.VerifiedProgress.Load())
				combined.ActiveWorkers.Store(connections)
				if expectedTotal <= 0 && partTotal > 0 {
					combined.SetTotalSize(baseBytes + partTotal)
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

func ensureWorkingFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("failed to create part working file: %w", err)
	}
	return file.Close()
}

func muxKind(kind string) mux.Kind {
	if kind == types.PartKindAudio {
		return mux.KindAudio
	}
	return mux.KindVideo
}
