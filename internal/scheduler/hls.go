package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
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

// fragmentMarkerName records which stream a fragment directory holds, so a
// directory left behind by a removed download cannot be mistaken for the
// resume state of a different one.
const fragmentMarkerName = ".stream"

// maxFragments caps how many fragments one download may fetch. An 8MB
// playlist can declare ~600k of them; each is an HTTP request, a file and a
// stat on every resume. A real feature-length stream is a few thousand.
const maxFragments = 50_000

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
	if len(fragments) > maxFragments {
		return 0, fmt.Errorf("%s: playlist declares %d fragments, more than the %d this download will fetch",
			cfg.ManifestURL, len(fragments), maxFragments)
	}

	if estimate := hls.EstimatedSize(media, bandwidth); estimate > 0 && progState != nil {
		// Fragment sizes are unknown until they arrive, so the bitrate-based
		// estimate is what gives the user a progress bar rather than a
		// counter. The real total replaces it once the file exists.
		progState.SetTotalSize(estimate)
	}

	fragDir := types.FragmentDirPath(finalDestPath)
	if err := prepareFragmentDir(fragDir, fragmentIdentity(cfg, fragments)); err != nil {
		return 0, err
	}

	if err := fetchFragments(ctx, client, cfg, progState, fragDir, fragments); err != nil {
		return 0, err
	}

	// Concatenate in playlist order, then let ffmpeg put the result in a real
	// container: a bare concatenation of transport-stream fragments plays, but
	// it is not the .mp4/.mkv the user asked for and carries no index.
	rawPath := types.ConcatWorkingPath(finalDestPath)
	if err := concatFragments(rawPath, fragDir, len(fragments)); err != nil {
		return 0, err
	}
	defer os.Remove(rawPath)

	muxTarget := types.MuxWorkingPath(finalDestPath)
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
	applyFragmentHeaders(req, cfg, credentialHost(cfg))

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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read playlist: %w", err)
	}
	if len(body) > maxManifestBytes {
		// Parsing a truncated playlist would download a prefix of the stream
		// and then call it complete.
		return nil, fmt.Errorf("playlist is larger than %d bytes; refusing to treat it as a playlist", maxManifestBytes)
	}
	return body, nil
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

	// The record's cancel function belongs to the scheduler's worker, which
	// cancels the context this one derives from on both pause and delete, so
	// there is nothing to install here - and installing it would leave the
	// state holding a dead function for the concat and remux that follow.
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if progState != nil {
		progState.ActiveWorkers.Store(int32(workers))
		defer progState.ActiveWorkers.Store(0)
	}

	var (
		next      atomic.Int64
		accounted atomic.Int64
		bytesGot  atomic.Int64
		failure   error
		failOnce  sync.Once
		wg        sync.WaitGroup
	)

	// Bytes already on disk from an earlier attempt count towards progress, so
	// a resumed stream does not restart the bar at zero. Those fragments count
	// towards the sample the size estimate is extrapolated from too, or the
	// first fetched fragment would look like it carried all of them.
	existing, existingCount := existingFragments(fragDir, len(fragments))
	if existingCount > 0 {
		bytesGot.Store(existing)
		accounted.Store(int64(existingCount))
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
				// A fragment already on disk was counted by the seed above;
				// counting it again would extrapolate the total from more
				// fragments than the bytes represent, publishing a total below
				// what is downloaded.
				count := accounted.Load()
				if written > 0 {
					count = accounted.Add(1)
				}
				if progState != nil {
					// Two workers can finish out of order; the byte count a
					// user watches must never tick backwards.
					storeMax(&progState.Bytes.Downloaded, total)
					storeMax(&progState.Bytes.VerifiedProgress, total)
					// With unknown fragment sizes the estimate can be beaten;
					// never report more than the total, and grow the total
					// once the estimate is clearly wrong.
					// count is 0 while another worker is between its byte
					// publication and its increment, and a zero-length
					// fragment contributes no bytes of its own.
					if _, knownTotal, _, _, _, _ := progState.GetProgress(); count > 0 && knownTotal > 0 && total > knownTotal {
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
	applyFragmentHeaders(req, cfg, credentialHost(cfg))
	ranged := fragment.Length > 0
	if ranged {
		// A byte-range fragment is a slice of a larger resource.
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", fragment.Offset, fragment.Offset+fragment.Length-1))
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fragment request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case ranged && resp.StatusCode != http.StatusPartialContent:
		// A 200 to a ranged request means the server sent the whole resource.
		// Storing it would put one full copy per fragment into the assembled
		// stream, and the rename would make that permanent.
		return 0, fmt.Errorf("fragment request failed: server ignored the byte range (status %d)", resp.StatusCode)
	case !ranged && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
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
	if ranged && written != fragment.Length {
		return 0, fmt.Errorf("fragment request failed: got %d bytes of the %d requested", written, fragment.Length)
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

// credentialHeaders travel with the site that issued them. A playlist is
// remote content and can name any host for its variants and fragments, so
// replaying a session cookie or a bearer token to that host would hand the
// user's credentials to whoever wrote the playlist.
var credentialHeaders = map[string]bool{
	"cookie":              true,
	"authorization":       true,
	"proxy-authorization": true,
}

// applyFragmentHeaders copies the download's headers onto a playlist or
// fragment request. issuer is the host the headers were issued for;
// anything else gets the request without them.
func applyFragmentHeaders(req *http.Request, cfg *types.DownloadRecord, issuer string) {
	sameHost := issuer == "" || strings.EqualFold(req.URL.Host, issuer)
	for key, value := range cfg.Headers {
		if strings.EqualFold(key, "Range") {
			continue
		}
		if !sameHost && credentialHeaders[strings.ToLower(key)] {
			continue
		}
		req.Header.Set(key, value)
	}
	if req.Header.Get("User-Agent") == "" && cfg.Runtime != nil {
		req.Header.Set("User-Agent", cfg.Runtime.GetUserAgent())
	}
}

// credentialHost is the host a download's headers belong to: the playlist the
// media is served from when there is one, else the page it was extracted from.
func credentialHost(cfg *types.DownloadRecord) string {
	// The playlist's host, not the page's: yt-dlp resolves the playlist URL
	// together with the headers, and media is usually served from a CDN host
	// that is not the page host. Stripping credentials there would fail the
	// very first request on any site that needs them.
	for _, candidate := range []string{cfg.ManifestURL, cfg.SourceURL} {
		if candidate == "" {
			continue
		}
		if parsed, err := neturl.Parse(candidate); err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return ""
}

func fragmentPath(fragDir string, index int) string {
	return filepath.Join(fragDir, fmt.Sprintf("%06d%s", index, fragmentSuffix))
}

// fragmentIdentity is what makes a fragment directory belong to one stream:
// the playlist it was built from and how many fragments that playlist had.
func fragmentIdentity(cfg *types.DownloadRecord, fragments []hls.Segment) string {
	// Not the playlist URL: it is re-signed on every resume, so hashing it
	// would discard the fragments of every paused download. The page the
	// stream was extracted from is the durable identity; a directly pasted
	// playlist has no page, and its URL is what the user typed.
	stream := cfg.SourceURL
	if stream == "" {
		stream = cfg.ManifestURL
	}
	// The format id comes with the page: a re-resolve that picks a different
	// rendition (a new variant published, a bandwidth tie broken the other
	// way) must not inherit fragments of the old one, because renditions are
	// segmented alike and would splice together without an error.
	sum := sha256.Sum256([]byte(stream + "\x00" + cfg.FormatID))
	return fmt.Sprintf("%x %d\n", sum[:16], len(fragments))
}

// prepareFragmentDir creates the fragment directory and makes sure whatever is
// already in it belongs to this download. A fragment is trusted purely because
// it is on disk, and the directory name is derived from the destination file,
// which the user can free by removing a failed download - so without this
// check a new stream saved under a recycled name would silently inherit the
// old fragments and assemble another video's data.
func prepareFragmentDir(fragDir, identity string) error {
	if err := os.MkdirAll(fragDir, 0o755); err != nil {
		return fmt.Errorf("failed to create fragment directory: %w", err)
	}

	markerPath := filepath.Join(fragDir, fragmentMarkerName)
	stored, readErr := os.ReadFile(markerPath)
	switch {
	case readErr == nil && string(stored) == identity:
		return nil
	case readErr == nil, !os.IsNotExist(readErr), !fragmentDirIsEmpty(fragDir):
		// A different stream's marker, an unreadable one, or fragments with no
		// marker at all: nothing here can be attributed to this download, and
		// a fragment is trusted purely because it exists.
		utils.Debug("Fragment directory %s does not belong to this stream; discarding it", fragDir)
		if err := os.RemoveAll(fragDir); err != nil {
			return fmt.Errorf("could not clear stale fragments: %w", err)
		}
		if err := os.MkdirAll(fragDir, 0o755); err != nil {
			return fmt.Errorf("failed to create fragment directory: %w", err)
		}
	}

	if err := os.WriteFile(markerPath, []byte(identity), 0o644); err != nil {
		return fmt.Errorf("could not record the fragment directory's stream: %w", err)
	}
	return nil
}

func fragmentDirIsEmpty(fragDir string) bool {
	entries, err := os.ReadDir(fragDir)
	if err != nil {
		return false
	}
	return len(entries) == 0
}

// existingFragments reports how many bytes of how many fragments an earlier
// attempt already left on disk.
func existingFragments(fragDir string, count int) (int64, int) {
	var total int64
	var found int
	for i := range count {
		if info, err := os.Stat(fragmentPath(fragDir, i)); err == nil {
			total += info.Size()
			found++
		}
	}
	return total, found
}

// storeMax publishes a monotonically increasing counter from concurrent
// writers without letting a late, smaller value undo a larger one.
func storeMax(counter *atomic.Int64, value int64) {
	for {
		current := counter.Load()
		if value <= current || counter.CompareAndSwap(current, value) {
			return
		}
	}
}

// concatFragments joins the fragments in playlist order. The order is the
// fragment index, not a sort of the names and not directory order: a name is
// only a rendering of the index, and a zero-padded number stops sorting
// correctly as soon as it needs one digit more than the padding.
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
