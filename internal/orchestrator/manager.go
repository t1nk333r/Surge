package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SurgeDM/Surge/internal/config"
	"github.com/SurgeDM/Surge/internal/extractor"
	"github.com/SurgeDM/Surge/internal/hls"
	"github.com/SurgeDM/Surge/internal/mux"
	probing "github.com/SurgeDM/Surge/internal/probe"
	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/store"
	"github.com/SurgeDM/Surge/internal/types"

	"github.com/SurgeDM/Surge/internal/scheduler"
	"github.com/SurgeDM/Surge/internal/utils"
)

// IsNameActiveFunc lets routing treat in-flight downloads as filename conflicts within a directory.
type IsNameActiveFunc func(dir, name string) bool

type LifecycleManager struct {
	settings            *config.Settings
	settingsMu          sync.RWMutex
	settingsRefreshedAt time.Time
	pool                *scheduler.Scheduler
	eventBus            *EventBus
	aggregator          *ProgressAggregator
	isNameActive        IsNameActiveFunc

	// mediaExtractor turns a media *page* URL into a direct media URL that the
	// normal download engine can fetch. Nil or unavailable means URLs are used
	// exactly as given, which is the pre-extraction behaviour.
	mediaExtractor extractor.Extractor

	// streamMuxer joins the separate video and audio streams of multi-part
	// media. Without it such media cannot be offered at all.
	streamMuxer mux.Muxer

	// probeSem caps the number of simultaneous server probes so adding a
	// large batch of downloads does not flood the network with HEAD requests.
	probeSem     chan struct{}
	shutdownOnce sync.Once
	inflightMu   sync.Mutex
	inflight     map[string]*inflightEnqueue
	// Test hooks make concurrent enqueue tests deterministic without affecting
	// production behavior.
	inflightJoinHook  func()
	inflightStartHook func()
}

type inflightEnqueue struct {
	done     chan struct{}
	id       string
	filename string
	err      error
}

const (
	maxWorkingFileReservationAttempts = 100
	// defaultMaxConcurrentProbes is the fallback probe concurrency cap used when
	// no settings value is available. The live value comes from
	// NetworkSettings.MaxConcurrentProbes.
	defaultMaxConcurrentProbes = 3
	// mediaExtractionTimeout caps one extractor run. yt-dlp needs a few
	// seconds for a normal page; anything beyond this is a hung tool.
	mediaExtractionTimeout = 90 * time.Second
)

var reserveWorkingFile = precreateWorkingFile

// freeDiskBytes is injectable so precheck tests can simulate tight disks.
var freeDiskBytes = utils.FreeDiskBytes

func precreateWorkingFile(destPath, filename string) error {
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	surgePath := filepath.Join(destPath, filename) + types.IncompleteSuffix
	// Exclusive create turns the .surge file into the reservation itself, so two
	// concurrent enqueues cannot silently target the same working path.
	file, err := os.OpenFile(surgePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to pre-create working file: %w", err)
	}
	_ = file.Close()
	return nil
}

// Falls back to a no-op so enqueue callers can always consult the active-name
// hook safely, even in tests or remote contexts that do not have pool access.
func (mgr *LifecycleManager) buildIsNameActive() func(string, string) bool {
	if mgr.isNameActive != nil {
		return mgr.isNameActive
	}
	return func(string, string) bool { return false }
}

func NewLifecycleManager(pool *scheduler.Scheduler, eventBus *EventBus, settings *config.Settings, isNameActive ...IsNameActiveFunc) *LifecycleManager {
	if settings == nil {
		settings = config.DefaultSettings()
	}

	var activeCheck IsNameActiveFunc
	if len(isNameActive) > 0 {
		activeCheck = isNameActive[0]
	}

	probeCap := defaultMaxConcurrentProbes
	if settings != nil && config.Resolve[int](settings.Network.MaxConcurrentProbes) > 0 {
		probeCap = config.Resolve[int](settings.Network.MaxConcurrentProbes)
	}
	sem := make(chan struct{}, probeCap)
	for i := 0; i < probeCap; i++ {
		sem <- struct{}{}
	}

	var aggregator *ProgressAggregator
	if pool != nil && eventBus != nil {
		aggregator = NewProgressAggregator(pool, eventBus, settings)
	}

	return &LifecycleManager{
		settings:            settings,
		settingsRefreshedAt: time.Now(),
		pool:                pool,
		eventBus:            eventBus,
		aggregator:          aggregator,
		isNameActive:        activeCheck,
		probeSem:            sem,
		inflight:            make(map[string]*inflightEnqueue),
		mediaExtractor:      extractor.NewYtDlp(),
		streamMuxer:         mux.NewFFmpeg(),
	}
}

// SetMediaExtractor replaces the media extractor. Passing nil disables
// extraction entirely.
func (mgr *LifecycleManager) SetMediaExtractor(ex extractor.Extractor) {
	mgr.mediaExtractor = ex
}

// GetScheduler returns the underlying scheduler
func (mgr *LifecycleManager) GetScheduler() *scheduler.Scheduler {
	return mgr.pool
}

// GetEventBus returns the event bus
func (mgr *LifecycleManager) GetEventBus() *EventBus {
	return mgr.eventBus
}

func (mgr *LifecycleManager) Shutdown() {
	mgr.shutdownOnce.Do(func() {
		if mgr.aggregator != nil {
			mgr.aggregator.Shutdown()
		}
		if mgr.pool != nil {
			mgr.pool.GracefulShutdown()
		}
		if mgr.eventBus != nil {
			mgr.eventBus.Shutdown()
		}
	})
}

func (m *LifecycleManager) GetSettings() *config.Settings {
	m.settingsMu.RLock()
	settings := m.settings
	m.settingsMu.RUnlock()

	if settings != nil {
		return settings
	}
	return config.DefaultSettings()
}

// ApplySettings swaps in a new routing snapshot for future enqueue calls.
func (m *LifecycleManager) ApplySettings(s *config.Settings) {
	if s == nil {
		return
	}
	m.settingsMu.Lock()
	m.settings = s
	m.settingsRefreshedAt = time.Now()
	m.settingsMu.Unlock()
}

// DownloadRequest carries the already-approved inputs needed to probe and reserve a file path.
type DownloadRequest struct {
	URL                string
	Filename           string
	Path               string
	Mirrors            []string
	Headers            map[string]string
	IsExplicitCategory bool
	SkipApproval       bool
	Workers            int
	MinChunkSize       int64

	// SourceURL and FormatID are set by media extraction: URL then holds the
	// resolved media URL and SourceURL the page it came from.
	SourceURL string
	FormatID  string

	// Parts is set for media that only exists as separate video and audio
	// streams; PartsTotalSize is their combined size, which is all the probe
	// can report for such an item.
	Parts          []types.DownloadPart
	PartsTotalSize int64

	// ManifestURL and ManifestSize are set for a fragmented (HLS) stream.
	// The size is a bitrate estimate at best.
	ManifestURL  string
	ManifestSize int64
}

// Enqueue probes and reserves a stable destination before dispatching to the queue layer.
func (mgr *LifecycleManager) Enqueue(ctx context.Context, req *DownloadRequest) (string, string, error) {
	if mgr.pool == nil {
		return "", "", types.ErrServiceUnavailable
	}

	utils.Debug("Lifecycle: Enqueue %s (Filename: %s)", req.URL, req.Filename)
	return mgr.enqueueResolved(ctx, req, "")
}

// EnqueueWithID does the same lifecycle work as Enqueue while preserving a caller-owned id.
func (mgr *LifecycleManager) EnqueueWithID(ctx context.Context, req *DownloadRequest, requestID string) (string, string, error) {
	if mgr.pool == nil {
		return "", "", types.ErrServiceUnavailable
	}

	utils.Debug("Lifecycle: EnqueueWithID %s (%s)", req.URL, requestID)
	return mgr.enqueueResolved(ctx, req, requestID)
}

func (mgr *LifecycleManager) enqueueResolved(ctx context.Context, req *DownloadRequest, requestID string) (string, string, error) {
	if req.URL == "" {
		return "", "", types.ErrURLRequired
	}
	if req.Path == "" {
		return "", "", types.ErrDestRequired
	}
	if requestID != "" {
		return mgr.enqueueNew(ctx, req, requestID)
	}

	key := enqueueCoalescingKey(req)
	mgr.inflightMu.Lock()
	if existing := mgr.inflight[key]; existing != nil {
		mgr.inflightMu.Unlock()
		if mgr.inflightJoinHook != nil {
			mgr.inflightJoinHook()
		}
		select {
		case <-existing.done:
			return existing.id, existing.filename, existing.err
		case <-ctx.Done():
			return "", "", fmt.Errorf("enqueue aborted while waiting for matching request: %w", ctx.Err())
		}
	}
	entry := &inflightEnqueue{done: make(chan struct{})}
	mgr.inflight[key] = entry
	mgr.inflightMu.Unlock()
	if mgr.inflightStartHook != nil {
		mgr.inflightStartHook()
	}

	entry.id, entry.filename, entry.err = mgr.enqueueNew(ctx, req, requestID)
	mgr.inflightMu.Lock()
	delete(mgr.inflight, key)
	close(entry.done)
	mgr.inflightMu.Unlock()
	return entry.id, entry.filename, entry.err
}

// enqueueCoalescingKey includes every request field that can affect the queued
// download. Requests that merely share a URL and destination may still have a
// different filename, authentication headers, or scheduling options and must
// therefore be enqueued independently.
func enqueueCoalescingKey(req *DownloadRequest) string {
	var key strings.Builder
	writeString := func(value string) {
		fmt.Fprintf(&key, "%d:%s", len(value), value)
	}
	writeString(req.URL)
	writeString(req.Filename)
	writeString(req.Path)
	fmt.Fprintf(&key, "%d:", len(req.Mirrors))
	for _, mirror := range req.Mirrors {
		writeString(mirror)
	}

	headerNames := make([]string, 0, len(req.Headers))
	for name := range req.Headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	fmt.Fprintf(&key, "%d:", len(headerNames))
	for _, name := range headerNames {
		writeString(name)
		writeString(req.Headers[name])
	}

	fmt.Fprintf(&key, "%t:%t:%d:%d", req.IsExplicitCategory, req.SkipApproval, req.Workers, req.MinChunkSize)
	return key.String()
}

func (mgr *LifecycleManager) enqueueNew(ctx context.Context, req *DownloadRequest, requestID string) (string, string, error) {
	settings := mgr.GetSettings()

	// Throttle concurrent probes — acquire a semaphore slot before probing.
	// If the context is cancelled (e.g., shutdown) we abort immediately.
	if mgr.probeSem != nil {
		select {
		case <-mgr.probeSem:
			// acquired
		case <-ctx.Done():
			return "", "", fmt.Errorf("enqueue aborted before probe: %w", ctx.Err())
		}
		defer func() { mgr.probeSem <- struct{}{} }()
	}

	probeResult, probeErr := probing.ProbeServerWithProxy(ctx, req.URL, req.Filename, req.Headers, settings.ToRuntimeConfig())
	if probeErr != nil {
		// Distinguish between terminal client errors (invalid scheme, etc.) and
		// server-side rejections or timeouts that we can optimistically ignore.
		var urlErr *neturl.Error
		var isTerminal bool
		if errors.As(probeErr, &urlErr) {
			var opErr *net.OpError
			isTerminal = !errors.As(probeErr, &opErr) && // not a network-layer error
				strings.Contains(urlErr.Error(), "unsupported protocol scheme")
		}
		isTerminal = isTerminal || errors.Is(probeErr, probing.ErrProbeRequestCreation)

		if isTerminal {
			return "", "", probeErr
		}

		utils.Debug("Lifecycle: Probe failed: %v - enqueueing with optimistic fallback metadata\n", probeErr)
		// Probe failures are non-fatal for known server-side issues (403/405/500) or
		// network timeouts: some servers reject lightweight probe requests but still
		// serve the actual download correctly.
		probeResult = &probing.ProbeResult{SupportsRange: true}
		if req.Filename != "" {
			probeResult.Filename = req.Filename
			probeResult.DetectedFilename = req.Filename
		}
	}

	// Try extraction when the probe fetched a web page rather than a file, and
	// also when the probe failed outright: sites that hand media pages to
	// browsers frequently reject a ranged probe, and saving that error page as
	// "the download" is never right.
	if extractor.LooksExtractable(probeResult.ContentType) || probeErr != nil {
		extracted, extractErr := mgr.extractMedia(ctx, req)
		switch {
		case extractErr != nil:
			return "", "", extractErr
		case extracted && (len(req.Parts) > 0 || req.ManifestURL != ""):
			// Neither shape has a single URL to probe: the engine works
			// through the parts or the playlist. The size is the best estimate
			// the extractor could give.
			size := req.PartsTotalSize
			if req.ManifestURL != "" {
				size = req.ManifestSize
			}
			probeResult = &probing.ProbeResult{
				FileSize:         size,
				Filename:         req.Filename,
				DetectedFilename: req.Filename,
			}
		case extracted:
			// Re-probe: the media URL is what the engine will actually fetch,
			// and its size and range support decide the strategy.
			probeResult, probeErr = probing.ProbeServerWithProxy(ctx, req.URL, req.Filename, req.Headers, settings.ToRuntimeConfig())
			if probeErr != nil {
				utils.Debug("Lifecycle: Probe of extracted media URL failed: %v", probeErr)
				probeResult = &probing.ProbeResult{SupportsRange: true}
				if req.Filename != "" {
					probeResult.Filename = req.Filename
					probeResult.DetectedFilename = req.Filename
				}
			}
		}
	}

	// A playlist URL pasted directly is a fragmented stream too: saving the
	// few kilobytes of text the server returns is never what the user meant.
	if req.ManifestURL == "" && len(req.Parts) == 0 && probeErr == nil &&
		hls.LooksLikePlaylist(probeResult.ContentType, req.URL) {
		if err := mgr.adoptPlaylistURL(req, probeResult); err != nil {
			return "", "", err
		}
		probeResult = &probing.ProbeResult{
			Filename:         req.Filename,
			DetectedFilename: req.Filename,
		}
	}

	isNameActive := mgr.buildIsNameActive()

	for attempt := 0; attempt < maxWorkingFileReservationAttempts; attempt++ {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("enqueue aborted: %w", ctx.Err())
		}

		finalPath, finalFilename, err := ResolveDestination(
			req.URL,
			req.Filename,
			req.Path,
			!req.IsExplicitCategory,
			settings,
			probeResult,
			isNameActive,
		)
		if err != nil {
			return "", "", fmt.Errorf("failed to resolve destination: %w", err)
		}

		// Known-size disk precheck before reservation so a reject leaves no
		// exclusive .surge orphan. Resume bypasses enqueueResolved by design.
		if probeResult != nil && probeResult.FileSize > 0 {
			free, freeErr := freeDiskBytes(finalPath)
			if freeErr != nil {
				utils.Debug("Lifecycle: free-space query failed for %s: %v (fail-open)", finalPath, freeErr)
			} else {
				buffer := settings.ToRuntimeConfig().GetPrecheckSafetyBuffer()
				if !utils.HasSufficientDiskSpace(probeResult.FileSize, free, buffer) {
					utils.Debug("Lifecycle: insufficient disk for %s (size=%d free=%d buffer=%d)", finalPath, probeResult.FileSize, free, buffer)
					return "", "", types.ErrInsufficientDiskSpace
				}
			}
		}

		// Reserve the working path before dispatch so a concurrent enqueue has to
		// pick a different name instead of truncating this in-flight download.
		if err := reserveWorkingFile(finalPath, finalFilename); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", "", err
		}

		surgePath := filepath.Join(finalPath, finalFilename) + types.IncompleteSuffix

		cfg, err := mgr.buildDownloadRecord(req, requestID, finalPath, finalFilename, probeResult)
		if err != nil {
			_ = os.Remove(surgePath)
			return "", "", err
		}

		queuedEvent := types.DownloadEvent{
			Type:         types.EventQueued,
			DownloadID:   cfg.ID,
			Filename:     finalFilename,
			URL:          req.URL,
			DestPath:     filepath.Join(finalPath, finalFilename),
			Mirrors:      append([]string(nil), req.Mirrors...),
			RateLimit:    cfg.RateLimit,
			RateLimitSet: cfg.RateLimitSet,
			Workers:      req.Workers,
			MinChunkSize: req.MinChunkSize,
		}
		// Persist synchronously before publishing so the download survives a restart
		// even if the event bus is full and Publish returns DeadlineExceeded.
		// Doing this BEFORE pool.Add prevents EventStarted from racing with this
		// persistence and corrupting the status to "queued" if it starts instantly.
		if err := store.AddToMasterList(types.DownloadRecord{
			ID:           queuedEvent.DownloadID,
			URL:          queuedEvent.URL,
			URLHash:      store.URLHash(queuedEvent.URL),
			DestPath:     queuedEvent.DestPath,
			Filename:     queuedEvent.Filename,
			Mirrors:      append([]string(nil), queuedEvent.Mirrors...),
			Status:       "queued",
			RateLimit:    queuedEvent.RateLimit,
			RateLimitSet: queuedEvent.RateLimitSet,
			Workers:      queuedEvent.Workers,
			MinChunkSize: queuedEvent.MinChunkSize,
		}); err != nil {
			utils.Debug("Lifecycle: Failed to persist queued download synchronously: %v", err)
		}
		if mgr.eventBus != nil {
			_ = mgr.eventBus.Publish(queuedEvent)
		}

		mgr.pool.Add(*cfg)

		return cfg.ID, finalFilename, nil
	}

	return "", "", fmt.Errorf("failed to reserve unique working file for %q after %d attempts", req.URL, maxWorkingFileReservationAttempts)
}

// extractMedia rewrites req in place when the URL is a media page the
// extractor can resolve to a single downloadable file. It reports whether the
// request was rewritten.
//
// Errors are deliberately asymmetric. A page the extractor does not recognise
// falls through so plain HTML downloads keep working, but a page it *does*
// recognise and cannot express as one file (a playlist, or only fragmented /
// split formats) is refused: saving the HTML or a silent video instead would
// be a wrong success.
func (mgr *LifecycleManager) extractMedia(ctx context.Context, req *DownloadRequest) (bool, error) {
	ex := mgr.mediaExtractor
	if ex == nil || !ex.Available() {
		return false, nil
	}

	extractCtx, cancel := context.WithTimeout(ctx, mediaExtractionTimeout)
	defer cancel()

	media, err := ex.Resolve(extractCtx, req.URL, mgr.extractorOptions())
	if err != nil {
		switch {
		case errors.Is(err, extractor.ErrPlaylist), errors.Is(err, extractor.ErrNoDirectFormat):
			return false, fmt.Errorf("%s: %w", req.URL, err)
		default:
			utils.Debug("Lifecycle: %s could not resolve %s: %v", ex.Name(), req.URL, err)
			return false, nil
		}
	}
	if media == nil || media.URL == "" {
		return false, nil
	}

	utils.Debug("Lifecycle: %s resolved %s to format %s (%d bytes)", ex.Name(), req.URL, media.FormatID, media.Size)

	req.SourceURL = media.SourceURL
	if req.SourceURL == "" {
		req.SourceURL = req.URL
	}
	req.FormatID = media.FormatID
	switch {
	case media.ManifestURL != "":
		// A fragmented stream is assembled with ffmpeg after the fragments
		// land, so without a muxer there is nothing to deliver.
		if muxer := mgr.streamMuxer; muxer == nil || !muxer.Available() {
			return false, fmt.Errorf("%s: %w: only a fragmented stream is offered; install ffmpeg to assemble it",
				req.URL, mux.ErrNotAvailable)
		}
		req.Parts = nil
		req.PartsTotalSize = 0
		req.ManifestURL = media.ManifestURL
		req.ManifestSize = media.Size
		req.URL = media.SourceURL
		// Fragment requests need the resolved headers, unlike a parted item
		// whose headers travel with each part.
		req.Headers = mergeRequestHeaders(req.Headers, media.Headers)

	case len(media.Parts) > 0:
		// Downloading the streams is pointless without the tool that joins
		// them, and half a video is not a successful download.
		if muxer := mgr.streamMuxer; muxer == nil || !muxer.Available() {
			return false, fmt.Errorf("%s: %w: only separate video and audio streams are offered; install ffmpeg to combine them",
				req.URL, mux.ErrNotAvailable)
		}
		// Nothing to probe or download directly: the record carries the page
		// URL for identity and the engine works through the parts. Part
		// headers travel with the parts.
		req.ManifestURL = ""
		req.Parts, req.PartsTotalSize = convertParts(media.Parts)
		req.URL = media.SourceURL
		req.Headers = nil

	default:
		req.Parts = nil
		req.PartsTotalSize = 0
		req.ManifestURL = ""
		req.URL = media.URL
		// Extracted URLs are signed for one client: the resolved headers
		// (User-Agent above all) are part of the credential and must win over
		// the caller's.
		req.Headers = mergeRequestHeaders(req.Headers, media.Headers)
	}
	if req.Filename == "" {
		req.Filename = media.Filename
	}
	// A media page and its resolved streams share nothing, so mirrors that
	// were meant for the page URL no longer apply.
	req.Mirrors = nil
	return true, nil
}

// adoptPlaylistURL turns a request for an HLS playlist into a fragmented
// download of that playlist. The name comes from the URL rather than the
// playlist (which carries no title), with a container extension, because
// "master.m3u8" is not a name for a video file.
func (mgr *LifecycleManager) adoptPlaylistURL(req *DownloadRequest, probeResult *probing.ProbeResult) error {
	if muxer := mgr.streamMuxer; muxer == nil || !muxer.Available() {
		return fmt.Errorf("%s: %w: assembling a fragmented stream needs ffmpeg", req.URL, mux.ErrNotAvailable)
	}

	req.ManifestURL = req.URL
	req.ManifestSize = 0
	req.Parts = nil
	req.PartsTotalSize = 0
	// Mirrors of a playlist do not serve its fragments.
	req.Mirrors = nil
	if req.Filename == "" {
		req.Filename = playlistFilename(req.URL)
	}
	utils.Debug("Lifecycle: treating %s as a fragmented stream", req.URL)
	return nil
}

// playlistFilename derives a media file name from a playlist URL: the last
// path element that names something, or the host, with an .mp4 extension
// since that is what HLS fragments carry.
func playlistFilename(rawurl string) string {
	parsed, err := neturl.Parse(rawurl)
	if err != nil {
		return "stream.mp4"
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		candidate := strings.TrimSuffix(strings.TrimSuffix(segments[i], ".m3u8"), ".m3u")
		switch strings.ToLower(candidate) {
		case "", "master", "index", "playlist", "manifest", "hls":
			// These name the playlist, not the media behind it.
			continue
		}
		return candidate + ".mp4"
	}
	if host := parsed.Hostname(); host != "" {
		return host + ".mp4"
	}
	return "stream.mp4"
}

func mergeRequestHeaders(base, override map[string]string) map[string]string {
	if len(base) == 0 {
		return override
	}
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// extractorOptions mirrors Surge's network policy onto the extractor's child
// process. The transport pool applies the configured proxy to every
// in-process request (internal/transport/network.go); an extractor runs
// outside it and would otherwise resolve media pages over a direct
// connection.
func (mgr *LifecycleManager) extractorOptions() extractor.Options {
	return extractor.Options{
		ProxyURL: mgr.GetSettings().ToRuntimeConfig().ProxyURL,
	}
}

// convertParts maps extractor parts onto the engine's record type and returns
// their combined size.
func convertParts(parts []extractor.Part) ([]types.DownloadPart, int64) {
	if len(parts) == 0 {
		return nil, 0
	}
	converted := make([]types.DownloadPart, 0, len(parts))
	var total int64
	for _, part := range parts {
		kind := types.PartKindVideo
		if part.Kind == extractor.KindAudio {
			kind = types.PartKindAudio
		}
		converted = append(converted, types.DownloadPart{
			URL:      part.URL,
			FormatID: part.FormatID,
			Kind:     kind,
			Size:     part.Size,
			Headers:  part.Headers,
		})
		total += part.Size
	}
	return converted, total
}

// IsNameActive reports whether the configured active-download callback would
// treat the given directory/name pair as an in-flight conflict.
func (mgr *LifecycleManager) IsNameActive(dir, name string) bool {
	return mgr.buildIsNameActive()(dir, name)
}

func (mgr *LifecycleManager) buildDownloadRecord(req *DownloadRequest, requestID string, finalPath string, finalFilename string, probeResult *probing.ProbeResult) (*types.DownloadRecord, error) {
	if mgr.pool == nil {
		return nil, types.ErrPoolNotInit
	}

	settings := mgr.GetSettings()
	id := strings.TrimSpace(requestID)
	if id == "" {
		id = uuid.New().String()
	}

	if st := mgr.pool.GetStatus(id); st != nil {
		return nil, types.ErrIDExists
	}

	state := progress.New(id, 0)
	state.SetDestPath(filepath.Join(finalPath, finalFilename))

	runtime := settings.ToRuntimeConfig()
	if req.Workers > 0 {
		maxConns := runtime.GetMaxConnectionsPerDownload()
		if req.Workers > maxConns {
			req.Workers = maxConns
		}
		runtime.Workers = req.Workers
	}
	if req.MinChunkSize > 0 {
		runtime.MinChunkSize = req.MinChunkSize
	}

	var rateLimit int64
	var rateLimitSet bool
	if settings.Network.DefaultDownloadRateLimit != nil {
		if parsed, err := utils.ParseRateLimitValue(settings.Network.DefaultDownloadRateLimit.Value); err == nil {
			rateLimit = parsed
			rateLimitSet = true
		}
	}

	cfg := types.DownloadRecord{
		URL:                req.URL,
		Mirrors:            req.Mirrors,
		OutputPath:         finalPath,
		ID:                 id,
		Filename:           finalFilename,
		SourceURL:          req.SourceURL,
		FormatID:           req.FormatID,
		Parts:              req.Parts,
		ManifestURL:        req.ManifestURL,
		ProgressState:      state,
		Runtime:            runtime,
		Headers:            req.Headers,
		IsExplicitCategory: req.IsExplicitCategory,
		TotalSize:          probeResult.FileSize,
		SupportsRange:      probeResult.SupportsRange,
		RateLimit:          rateLimit,
		RateLimitSet:       rateLimitSet,
	}

	if mgr.eventBus != nil {
		cfg.ProgressCh = mgr.eventBus.InputCh
	}

	return &cfg, nil
}
