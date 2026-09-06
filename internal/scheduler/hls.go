package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/SurgeDM/Surge/internal/hls"
	"github.com/SurgeDM/Surge/internal/mux"
	"github.com/SurgeDM/Surge/internal/progress"
	"github.com/SurgeDM/Surge/internal/transport"
	"github.com/SurgeDM/Surge/internal/types"
	"github.com/SurgeDM/Surge/internal/utils"
)

// maxManifestBytes caps a playlist read. Real playlists are kilobytes; a
// multi-megabyte response is a misconfigured server or a redirect to a media
// file, and reading it whole would be pointless.
const maxManifestBytes = 8 << 20

// fragmentSuffix marks a fully downloaded fragment. Fragments are written to a
// temporary name and renamed, so the presence of this file — not its size — is
// what says "done", which is what makes a resumed HLS download skip work.
const fragmentSuffix = ".frag"

// ErrSeparateAudioRendition is returned for streams whose audio is a separate
// rendition rather than part of the selected variant's segments. Downloading
// only the variant would produce a silent video, which is not a successful
// download; combining renditions is not implemented yet.
var ErrSeparateAudioRendition = errors.New("stream keeps its audio in a separate rendition")

// runManifestDownload downloads an HLS stream: it reads the playlist, fetches
// every fragment through the same pooled (proxy-aware, rate-limited)
// transport the rest of Surge uses, concatenates them in playlist order and
// remuxes the result into the reserved working file.
//
// Fragments are plain HTTP files, so nothing about the engine changes; the only
// new thing is that one download is many requests whose sizes are not known up
// front.
func runManifestDownload(ctx context.Context, cfg *types.DownloadRecord, progState *progress.DownloadProgress, finalDestPath string) (int64, error) {
	if defaultMuxer == nil || !defaultMuxer.Available() {
		return 0, fmt.Errorf("%w: cannot assemble a fragmented stream", mux.ErrNotAvailable)
	}

	httpTransport := transport.DefaultNetworkPool.AcquireTransport(cfg.Runtime.ProxyURL, cfg.Runtime.CustomDNS, 0)
	defer transport.DefaultNetworkPool.ReleaseTransport(httpTransport)
	client := &http.Client{Transport: httpTransport}

	media, bandwidth, err := resolveMediaPlaylist(ctx, client, cfg)
	if err != nil {
		return 0, err
	}

	fragments := make([]hls.Segment, 0, len(media.Segments)+1)
	if media.InitSection != nil {
		// The fMP4 initialisation segment carries the codec configuration and
		// is worthless out of order, so it is simply fragment zero.
		fragments = append(fragments, *media.InitSection)
	}
	fragments = append(fragments, media.Segments...)
	if len(fragments) == 0 {
		return 0, fmt.Errorf("%s: playlist lists no fragments", cfg.URL)
	}

	if estimate := hls.EstimatedSize(media, bandwidth); estimate > 0 && progState != nil {
		// Fragment sizes are unknown until they arrive, so the bitrate-based
		// estimate is what gives the user a progress bar rather than a
		// counter. The real total replaces it once the file exists.
		progState.SetTotalSize(estimate)
	}

	fragDir := finalDestPath + ".frags"
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		return 0, fmt.Errorf("failed to create fragment directory: %w", err)
	}

	if err := fetchFragments(ctx, client, cfg, progState, fragDir, fragments); err != nil {
		return 0, err
	}

	// Concatenate in playlist order, then let ffmpeg put the result in a real
	// container: a bare concatenation of transport-stream fragments plays, but
	// it is not the .mp4/.mkv the user asked for and carries no index.
	rawPath := finalDestPath + ".hlsraw"
	if err := concatFragments(rawPath, fragDir, len(fragments)); err != nil {
		return 0, err
	}
	defer os.Remove(rawPath)

	muxTarget := finalDestPath + ".muxing" + filepath.Ext(finalDestPath)
	utils.Debug("Remuxing %d fragments into %s", len(fragments), muxTarget)
	if err := defaultMuxer.Remux(ctx, muxTarget, rawPath); err != nil {
		return 0, fmt.Errorf("assembling the downloaded fragments failed: %w", err)
	}

	target := finalDestPath + types.IncompleteSuffix
	if err := os.Rename(muxTarget, target); err != nil {
		_ = os.Remove(muxTarget)
		return 0, fmt.Errorf("could not move the assembled file into place: %w", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		utils.Debug("Could not normalise permissions on %s: %v", target, err)
	}

	// Only now are the fragments redundant. Keeping them until the remux
	// succeeds means a failed assembly can be retried without re-downloading.
	if err := os.RemoveAll(fragDir); err != nil {
		utils.Debug("Could not remove fragment directory %s: %v", fragDir, err)
	}

	finalSize := int64(0)
	if info, err := os.Stat(target); err == nil {
		finalSize = info.Size()
	}
	if progState != nil && finalSize > 0 {
		progState.SetTotalSize(finalSize)
		progState.Bytes.Downloaded.Store(finalSize)
		progState.Bytes.VerifiedProgress.Store(finalSize)
	}
	return finalSize, nil
}

// resolveMediaPlaylist fetches cfg.ManifestURL and, when it is a master
// playlist, follows the best variant. It returns the media playlist and the
// bandwidth the variant advertised (0 when unknown), which is the only size
// hint an HLS stream offers.
func resolveMediaPlaylist(ctx context.Context, client *http.Client, cfg *types.DownloadRecord) (*hls.Playlist, int, error) {
	manifestURL := cfg.ManifestURL
	body, err := fetchManifest(ctx, client, cfg, manifestURL)
	if err != nil {
		return nil, 0, err
	}

	playlist, err := hls.Parse(body, manifestURL)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", manifestURL, err)
	}

	if len(playlist.Segments) > 0 {
		return playlist, 0, nil
	}

	variant := hls.BestVariant(playlist)
	if variant == nil {
		return nil, 0, fmt.Errorf("%s: %w", manifestURL, hls.ErrEmptyPlaylist)
	}
	// A variant that names an audio group has no audio in its own segments.
	if variant.Audio != "" || audioRenditionFor(playlist, variant) != nil {
		return nil, 0, fmt.Errorf("%s: %w", manifestURL, ErrSeparateAudioRendition)
	}

	body, err = fetchManifest(ctx, client, cfg, variant.URL)
	if err != nil {
		return nil, 0, err
	}
	media, err := hls.Parse(body, variant.URL)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", variant.URL, err)
	}
	if len(media.Segments) == 0 {
		return nil, 0, fmt.Errorf("%s: %w", variant.URL, hls.ErrEmptyPlaylist)
	}
	return media, variant.Bandwidth, nil
}

// audioRenditionFor reports the audio rendition a variant depends on, if the
// playlist declares one with its own URL.
func audioRenditionFor(playlist *hls.Playlist, variant *hls.Variant) *hls.Rendition {
	for i := range playlist.AudioRenditions {
		rendition := &playlist.AudioRenditions[i]
		if rendition.URL == "" {
			// Muxed into the video segments: nothing separate to fetch.
			continue
		}
		if variant.Audio == "" || rendition.GroupID == variant.Audio {
			return rendition
		}
	}
	return nil
}

func fetchManifest(ctx context.Context, client *http.Client, cfg *types.DownloadRecord, rawurl string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, fmt.Errorf("could not request playlist: %w", err)
	}
	applyFragmentHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch playlist: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("playlist request failed: unexpected status code: %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
}

// fetchFragments downloads every fragment that is not already on disk, using
// the download's worker budget for concurrency and its limiter for rate
// limiting. Order does not matter here — the concatenation reads by index.
func fetchFragments(
	ctx context.Context,
	client *http.Client,
	cfg *types.DownloadRecord,
	progState *progress.DownloadProgress,
	fragDir string,
	fragments []hls.Segment,
) error {
	workers := cfg.Runtime.GetWorkers()
	if workers <= 0 {
		workers = 4
	}
	if workers > len(fragments) {
		workers = len(fragments)
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if progState != nil {
		progState.SetCancelFunc(cancel)
		progState.ActiveWorkers.Store(int32(workers))
		defer progState.ActiveWorkers.Store(0)
	}

	var (
		next     atomic.Int64
		done     atomic.Int64
		bytesGot atomic.Int64
		failure  error
		failOnce sync.Once
		wg       sync.WaitGroup
	)

	// Bytes already on disk from an earlier attempt count towards progress, so
	// a resumed stream does not restart the bar at zero.
	if existing := existingFragmentBytes(fragDir, len(fragments)); existing > 0 {
		bytesGot.Store(existing)
		if progState != nil {
			progState.Bytes.Downloaded.Store(existing)
			progState.Bytes.VerifiedProgress.Store(existing)
		}
	}

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= len(fragments) {
					return
				}
				if fetchCtx.Err() != nil {
					return
				}

				written, err := fetchFragment(fetchCtx, client, cfg, fragments[index], fragmentPath(fragDir, index))
				if err != nil {
					failOnce.Do(func() { failure = err })
					cancel()
					return
				}

				total := bytesGot.Add(written)
				count := done.Add(1)
				if progState != nil {
					progState.Bytes.Downloaded.Store(total)
					progState.Bytes.VerifiedProgress.Store(total)
					// With unknown fragment sizes the estimate can be beaten;
					// never report more than the total, and grow the total
					// once the estimate is clearly wrong.
					if _, knownTotal, _, _, _, _ := progState.GetProgress(); knownTotal > 0 && total > knownTotal {
						progState.SetTotalSize(total * int64(len(fragments)) / count)
					}
				}
			}
		}()
	}
	wg.Wait()

	if failure != nil {
		return failure
	}
	// A pause cancels the context; report it as a pause so the scheduler keeps
	// the fragments and the download can be resumed.
	if progState != nil && (progState.IsPaused() || progState.IsPausing()) {
		return types.ErrPaused
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// fetchFragment downloads one fragment unless it is already there. It writes to
// a temporary file and renames, so a partial fragment can never be mistaken for
// a complete one.
func fetchFragment(ctx context.Context, client *http.Client, cfg *types.DownloadRecord, fragment hls.Segment, path string) (int64, error) {
	if _, err := os.Stat(path); err == nil {
		return 0, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fragment.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("could not request fragment: %w", err)
	}
	applyFragmentHeaders(req, cfg)
	if fragment.Length > 0 {
		// A byte-range fragment is a slice of a larger resource.
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", fragment.Offset, fragment.Offset+fragment.Length-1))
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fragment request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("fragment request failed: unexpected status code: %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".surge-frag-*")
	if err != nil {
		return 0, fmt.Errorf("could not create fragment file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	var reader io.Reader = resp.Body
	if cfg.Limiter != nil {
		// The same limiter object the byte-range downloaders use, so a global
		// or per-download rate limit applies to fragments identically.
		reader = &limitedReader{reader: resp.Body, limiter: cfg.Limiter, ctx: ctx}
	}

	written, copyErr := io.Copy(tmp, reader)
	closeErr := tmp.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("fragment download failed: %w", copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("could not finish fragment file: %w", closeErr)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return 0, fmt.Errorf("could not store fragment: %w", err)
	}
	return written, nil
}

// limitedReader charges every read to the download's byte limiter.
type limitedReader struct {
	reader  io.Reader
	limiter types.ByteLimiter
	ctx     context.Context
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.reader.Read(p)
	if n > 0 && l.limiter != nil {
		if waitErr := l.limiter.WaitN(l.ctx, int64(n)); waitErr != nil {
			if err != nil && !errors.Is(err, io.EOF) {
				return n, err
			}
			return n, waitErr
		}
	}
	return n, err
}

func applyFragmentHeaders(req *http.Request, cfg *types.DownloadRecord) {
	for key, value := range cfg.Headers {
		if strings.EqualFold(key, "Range") {
			continue
		}
		req.Header.Set(key, value)
	}
	if req.Header.Get("User-Agent") == "" && cfg.Runtime != nil {
		req.Header.Set("User-Agent", cfg.Runtime.GetUserAgent())
	}
}

func fragmentPath(fragDir string, index int) string {
	return filepath.Join(fragDir, fmt.Sprintf("%06d%s", index, fragmentSuffix))
}

func existingFragmentBytes(fragDir string, count int) int64 {
	var total int64
	for i := range count {
		if info, err := os.Stat(fragmentPath(fragDir, i)); err == nil {
			total += info.Size()
		}
	}
	return total
}

// concatFragments joins the fragments in playlist order. Order is taken from
// the index in each name rather than from directory order, which is not sorted
// on every filesystem.
func concatFragments(rawPath, fragDir string, count int) error {
	out, err := os.Create(rawPath)
	if err != nil {
		return fmt.Errorf("could not create assembled file: %w", err)
	}
	defer out.Close()

	names := make([]string, 0, count)
	for i := range count {
		names = append(names, fragmentPath(fragDir, i))
	}
	sort.Strings(names)

	for _, name := range names {
		fragment, err := os.Open(name)
		if err != nil {
			return fmt.Errorf("missing fragment %s: %w", filepath.Base(name), err)
		}
		_, copyErr := io.Copy(out, fragment)
		closeErr := fragment.Close()
		if copyErr != nil {
			return fmt.Errorf("could not append fragment %s: %w", filepath.Base(name), copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("could not close fragment %s: %w", filepath.Base(name), closeErr)
		}
	}
	return out.Sync()
}
