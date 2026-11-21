package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/types"
	"github.com/SurgeDM/Surge/internal/utils"
)

const terminalSendTimeout = 3 * time.Second

// safeSendProgress sends msg on ch, recovering from panics caused by sending
// on a closed channel (which can happen during shutdown).
func safeSendProgress(ch chan<- types.DownloadEvent, msg types.DownloadEvent, doneCh <-chan struct{}) {
	defer func() { _ = recover() }()
	select {
	case ch <- msg:
		return
	default:
	}
	timer := time.NewTimer(terminalSendTimeout)
	defer timer.Stop()
	select {
	case ch <- msg:
	case <-doneCh:
	case <-timer.C:
		utils.Debug("safeSendProgress: timed out delivering event %v", msg.Type)
	}
}

// uniqueFilePath returns a unique file path by appending (1), (2), etc. if the file exists
func uniqueFilePath(path string) string {
	// Check if file exists (both final and incomplete)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if _, err := os.Stat(path + types.IncompleteSuffix); os.IsNotExist(err) {
			return path // Neither exists, use original
		}
	}

	// File exists, generate unique name
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	name := strings.TrimSuffix(filepath.Base(path), ext)

	// Check if name already has a counter like "file(1)"
	base := name
	counter := 1

	// Clean name to ensure parsing works even with trailing spaces
	cleanName := strings.TrimSpace(name)
	if len(cleanName) > 3 && cleanName[len(cleanName)-1] == ')' {
		if openParen := strings.LastIndexByte(cleanName, '('); openParen != -1 {
			// Try to parse number between parens
			numStr := cleanName[openParen+1 : len(cleanName)-1]
			if num, err := strconv.Atoi(numStr); err == nil && num > 0 {
				base = cleanName[:openParen]
				// Parsing "file (1)" -> "file " preserves original whitespace.
				counter = num + 1
			}
		}
	}

	for i := 0; i < 100; i++ { // Try next 100 numbers
		candidate := filepath.Join(dir, fmt.Sprintf("%s(%d)%s", base, counter+i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			if _, err := os.Stat(candidate + types.IncompleteSuffix); os.IsNotExist(err) {
				return candidate
			}
		}
	}

	// Fallback: if all 100 numbered candidates are taken, return an empty string so
	// callers can detect the failure rather than silently receiving a conflicting path.
	return ""
}

// RunDownload is the main entry point for downloads executed by the Engine pool
func RunDownload(ctx context.Context, cfg *types.DownloadRecord) error {
	start := time.Now()
	if cfg.Runtime == nil {
		cfg.Runtime = types.DefaultRuntimeConfig()
	}
	// Engine expects cfg.OutputPath and cfg.Filename to be fully resolved by the processing layer
	destPath := cfg.OutputPath
	finalFilename := cfg.Filename
	finalDestPath := filepath.Join(destPath, finalFilename)

	// Local mirrors slice to avoid modifying config (race condition)
	mirrors := append([]string(nil), cfg.Mirrors...)

	// Check if this is a resume (explicitly marked by TUI)
	var savedState *types.DownloadRecord

	if cfg.IsResume && cfg.DestPath != "" {
		savedState = cfg

		// Restore mirrors from state if found
		if savedState != nil && len(savedState.Mirrors) > 0 {
			// Create map of existing mirrors to avoid duplicates
			existing := make(map[string]bool)
			for _, m := range mirrors {
				existing[m] = true
			}

			// Add restored mirrors
			for _, m := range savedState.Mirrors {
				if !existing[m] {
					mirrors = append(mirrors, m)
					existing[m] = true
				}
			}
			utils.Debug("Restored %d mirrors from state", len(savedState.Mirrors))
		}
	}
	isResume := cfg.IsResume && savedState != nil && savedState.DestPath != ""

	if isResume {
		// Resume: use saved destination path directly (don't generate new unique name)
		finalDestPath = savedState.DestPath
		finalFilename = filepath.Base(finalDestPath)
		utils.Debug("Resuming download, using saved destPath: %s", finalDestPath)
	}
	// A cleanup failure or a crash can leave both published and working names
	// pointing to one inode. Never open that working name for another transfer.
	published, err := utils.SameFilePaths(finalDestPath, finalDestPath+types.IncompleteSuffix)
	if err != nil {
		return err
	}
	if published {
		return types.ErrCompleted
	}
	utils.Debug("Destination path: %s", finalDestPath)

	var progState *progress.DownloadProgress
	if cfg.ProgressState != nil {
		progState = progress.CfgProgress(cfg)
		progState.SetFilename(finalFilename)
		progState.SetDestPath(finalDestPath)
	}

	currentRateLimit := func() (int64, bool) {
		if progState != nil {
			return progState.GetRateLimit()
		}
		return cfg.RateLimit, cfg.RateLimitSet
	}

	// Send download started message
	if cfg.ProgressCh != nil {
		rateLimit, rateLimitSet := currentRateLimit()
		safeSendProgress(cfg.ProgressCh, types.DownloadEvent{
			Type: types.EventStarted, Artifact: cfg.Artifact,
			DownloadID:   cfg.ID,
			URL:          cfg.URL,
			Filename:     finalFilename,
			Total:        cfg.TotalSize, // Relies on TotalSize from Config
			DestPath:     finalDestPath,
			State:        cfg,
			RateLimit:    rateLimit,
			RateLimitSet: rateLimitSet,
			Workers:      cfg.Runtime.Workers,
			MinChunkSize: cfg.Runtime.MinChunkSize,
		}, ctx.Done())
	}

	// Update shared state if we have a valid size
	if progState != nil && cfg.TotalSize > 0 {
		progState.SetTotalSize(cfg.TotalSize)
	}

	effectiveTotalSize := cfg.TotalSize
	if progState != nil && effectiveTotalSize <= 0 {
		_, stateTotal, _, _, _, _ := progState.GetProgress()
		if stateTotal > 0 {
			effectiveTotalSize = stateTotal
		}
	}

	// Three shapes of download: a fragmented stream assembled from a playlist,
	// separate video and audio streams that are muxed, or one plain HTTP file.
	var downloadErr error
	switch {
	case cfg.ManifestURL != "":
		// A fragmented stream (HLS): many requests, one file.
		var manifestSize int64
		manifestSize, downloadErr = runManifestDownload(ctx, cfg, progState, finalDestPath)
		if manifestSize > 0 {
			effectiveTotalSize = manifestSize
		}
	case len(cfg.Parts) > 0:
		var partedSize int64
		partedSize, downloadErr = runPartedDownload(ctx, cfg, progState, finalDestPath)
		if partedSize > 0 {
			effectiveTotalSize = partedSize
		}
	default:
		var size int64
		size, downloadErr = acquireStream(ctx, cfg, progState, streamSpec{
			url:           cfg.URL,
			headers:       cfg.Headers,
			destPath:      finalDestPath,
			filename:      finalFilename,
			totalSize:     effectiveTotalSize,
			supportsRange: cfg.SupportsRange,
			mirrors:       mirrors,
			id:            cfg.ID,
			progressCh:    cfg.ProgressCh,
		})
		if size > 0 {
			effectiveTotalSize = size
		}
	}

	// The media paths never publish EventPaused for the record (their streams
	// run under their own IDs), while a typed ErrPaused tells the scheduler that
	// event was already queued. Report their pause as a cancellation so the
	// scheduler sends the record's fallback EventPaused.
	if (cfg.ManifestURL != "" || len(cfg.Parts) > 0) && errors.Is(downloadErr, types.ErrPaused) {
		utils.Debug("Media download paused cleanly")
		return context.Canceled
	}

	// Return typed pause error rather than normalizing to nil so callers
	// can distinguish a clean pause from successful completion or errors.
	if errors.Is(downloadErr, types.ErrPaused) {
		utils.Debug("Download paused cleanly")
		return downloadErr
	}

	// Physical download success takes precedence over late-arriving pause signals.
	if downloadErr == nil {
		// The response length may be unknown or differ from the completed file.
		// Completion is emitted before the lifecycle promotes the working file.
		if info, err := os.Stat(finalDestPath + types.IncompleteSuffix); err == nil && info.Mode().IsRegular() {
			effectiveTotalSize = info.Size()
		} else if info, err := os.Stat(finalDestPath); err == nil && info.Mode().IsRegular() {
			effectiveTotalSize = info.Size()
		}
		cfg.TotalSize = effectiveTotalSize
		if cfg.Artifact.ExpectedSHA256 != "" {
			if progState != nil {
				progState.Verifying.Store(true)
			}
			cfg.Artifact.Verification = "verifying"
			if cfg.ProgressCh != nil {
				safeSendProgress(cfg.ProgressCh, types.DownloadEvent{Type: types.EventSystem, DownloadID: cfg.ID, Message: "Verifying SHA-256", Artifact: cfg.Artifact}, ctx.Done())
			}
			digest, err := hashArtifact(ctx, finalDestPath+types.IncompleteSuffix)
			if progState != nil {
				progState.Verifying.Store(false)
			}
			if err != nil {
				return err
			}
			cfg.Artifact.SHA256 = digest
			if digest != cfg.Artifact.ExpectedSHA256 {
				cfg.Artifact.Verification = "mismatch"
				return types.ErrIntegrityMismatch
			}
			cfg.Artifact.Verification = "verified"
		}
		var elapsed time.Duration
		if progState != nil {
			_, elapsed = progState.FinalizeSession(effectiveTotalSize)
		} else {
			elapsed = time.Since(start)
		}

		// Persist to history before sending event
		// Compute average download speed in bytes/sec
		var avgSpeed float64
		if elapsed.Seconds() > 0 {
			avgSpeed = float64(effectiveTotalSize) / elapsed.Seconds()
		}

		if cfg.ProgressCh != nil {
			rateLimit, rateLimitSet := currentRateLimit()
			// EventComplete is terminal-reliable and must not be dropped due to canceled context.
			safeSendProgress(cfg.ProgressCh, types.DownloadEvent{
				Type: types.EventComplete, Artifact: cfg.Artifact,
				DownloadID:   cfg.ID,
				Filename:     finalFilename,
				Elapsed:      elapsed,
				Total:        effectiveTotalSize,
				AvgSpeed:     avgSpeed,
				RateLimit:    rateLimit,
				RateLimitSet: rateLimitSet,
			}, nil)
		}
		return nil
	}

	if errors.Is(downloadErr, context.Canceled) || errors.Is(downloadErr, context.DeadlineExceeded) {
		utils.Debug("Download canceled cleanly")
		return downloadErr
	}

	return downloadErr
}

// shouldFallbackToSingle permits a restart only for confirmed range rejection.
// Pause, cancellation and disk-full errors take precedence in joined errors.
func shouldFallbackToSingle(downloadErr error, downloaded int64) bool {
	if downloadErr == nil {
		return false
	}
	if errors.Is(downloadErr, types.ErrPaused) {
		return false
	}
	if errors.Is(downloadErr, context.Canceled) || errors.Is(downloadErr, context.DeadlineExceeded) {
		return false
	}
	if types.IsInsufficientDiskSpace(downloadErr) {
		return false
	}
	if errors.Is(downloadErr, types.ErrRangeUnsupported) {
		return true
	}
	return false
}

// Download is the CLI entry point (non-TUI) - convenience wrapper
func Download(ctx context.Context, url string, outPath string, progressCh chan<- types.DownloadEvent, id string) error {
	cfg := types.DownloadRecord{
		URL:           url,
		OutputPath:    outPath,
		ID:            id,
		ProgressCh:    progressCh,
		ProgressState: nil,
	}
	// Default runtime config
	cfg.Runtime = types.DefaultRuntimeConfig()
	return RunDownload(ctx, &cfg)
}
