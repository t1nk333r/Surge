# Legacy usage guide

The task-oriented documentation has moved to the [documentation index](README.md).
For the current command list and flags, see the [CLI reference](reference/cli.md).

# Usage Guide

Surge has both a robust CLI and a fast Interactive TUI. For configuration options, see [SETTINGS.md](SETTINGS.md).

## TUI Usage

In the interactive TUI (launched simply via `surge`), you can manage your downloads through keyboard shortcuts. Press `h` to open or close the keyboard-shortcuts overlay. Press `?` to report a bug.

### Adding Downloads from Clipboard

While in the TUI Dashboard, you can rapidly add downloads using your clipboard:
- Press `a` to manually type or paste a URL.
- Press `Shift+A` to directly attempt to parse a copied browser **cURL** command (from "Copy as cURL"). Surge will extract the URL and all headers (like cookies and user-agents).

## CLI Commands

## Command Table

| Command                     | What it does                                                                           | Key flags                                                                                           | Notes                                                                   |
| :-------------------------- | :------------------------------------------------------------------------------------- | :-------------------------------------------------------------------------------------------------- | :---------------------------------------------------------------------- |
| `surge [url]...`            | Launches local TUI. Queues optional URLs.                                              | `--batch, -b`<br>`--port, -p`<br>`--output, -o`<br>`--no-resume`<br>`--exit-when-done`<br>`--no-server` | `-o` defaults to CWD. If `--host` is set, this becomes remote TUI mode. `--no-server` disables the embedded HTTP API for that session. |
| `surge server [url]...`     | Launches headless server. Queues optional URLs.                                        | `--batch, -b`<br>`--port, -p`<br>`--bind`<br>`--output, -o`<br>`--exit-when-done`<br>`--no-resume`<br>`--token` | `--bind` defaults to `0.0.0.0`; use `127.0.0.1` for local-only access. |
| `surge connect [host:port]` | Launches TUI connected to a server. Auto-detects local server when no target is given. | `--insecure-http`                                                                                   | Convenience alias for remote TUI usage.                                 |
| `surge add <url>...`        | Queues downloads via CLI/API.                                                          | `--batch, -b`<br>`--output, -o`                                                                     | `-o` defaults to CWD. Alias: `get`.                                     |
| `surge ls [id]`             | Lists downloads, or shows one download detail.                                         | `--json`<br>`--watch`<br>`--server-only`                                                          | Alias: `l`. `--server-only` requires a live server and disables the database fallback. |
| `surge limit <id> <speed>`  | Sets per-download, global, or default speed limits.                                    | `--global`<br>`--default`                                                                           | Use `unlimited`/`0` to disable, or `inherit` for per-download default.   |
| `surge pause <id>`          | Pauses a download by ID/prefix.                                                        | `--all`                                                                                             |                                                                         |
| `surge resume <id>`         | Resumes a paused download by ID/prefix.                                                | `--all`                                                                                             |                                                                         |
| `surge refresh <id> <url>`  | Updates the source URL of a paused or errored download.                                | None                                                                                                | Reconnects using the new link.                                          |
| `surge rm <id>`             | Removes a download by ID/prefix.                                                       | `--clean`, `--purge`                                                                                | Alias: `kill`.                                                          |
| `surge config [path] [val]` | Get, set, or reset Surge configuration options via the CLI.                            | None                                                                                                | See [SETTINGS.md](SETTINGS.md) for available settings. Run without args to list all. |
| `surge token`               | Prints current API auth token. (Also visible in TUI > Settings > Extension)            | None                                                                                                | Useful for remote clients.                                              |
| `surge service <cmd>`       | Manages Surge as a system service (daemon).                                            | `install`, `uninstall`, `start`, `stop`, `status`, `token`                                      | Cross-platform (Linux/Windows/macOS). See [Service Management](#service-management). |
| `surge bug-report`          | Opens a pre-filled GitHub bug report. Prompts for target (Core/Extension) and optional system/log details. | None                                                                                                | Prints a manual URL fallback if browser open fails.                     |

## Service Management

The `service` command allows you to manage Surge as a background daemon that starts automatically on boot.

- `surge service install`: Registers Surge as a system service.
- `surge service uninstall`: Removes the system service.
- `surge service start`: Starts the background service.
- `surge service stop`: Stops the background service.
- `surge service status`: Checks if the service is installed and running.
- `surge service token`: Prints the auth token used by the system service daemon.

**Note**: On most systems, these commands require administrative privileges (e.g., `sudo surge service install`).

## Server Subcommands (Compatibility)

| Command                       | What it does                                           |
| :---------------------------- | :----------------------------------------------------- |
| `surge server start [url]...` | Legacy equivalent of `surge server [url]...`.          |
| `surge server stop`           | Stops a running server process by PID file.            |
| `surge server status`         | Prints running/not-running status from PID/port state. |

## Global Flags

These are persistent flags and can be used with all commands.

| Flag                 | Description                            |
| :------------------- | :------------------------------------- |
| `--host <host:port>` | Target server for TUI and CLI actions. |
| `--token <token>`    | Bearer token used for API requests.    |

## Environment Variables

| Variable      | Description                                   |
| :------------ | :-------------------------------------------- |
| `SURGE_HOST`  | Default host when `--host` is not provided.   |
| `SURGE_TOKEN` | Default token when `--token` is not provided. |
| `SURGE_YTDLP` | Path to the `yt-dlp` binary used for media extraction. |
| `SURGE_FFMPEG` | Path to the `ffmpeg` binary used to join separate video and audio streams. |

## Media Pages (YouTube and friends)

Adding the URL of a media *page* rather than a file used to save the HTML.
When [`yt-dlp`](https://github.com/yt-dlp/yt-dlp) is installed and on `PATH`,
Surge now asks it which media the page points at and downloads that instead:

```bash
surge add -- "https://www.youtube.com/watch?v=..."
```

The download itself stays in Surge. yt-dlp only answers "which URL and which
headers", so the media file is fetched by the same engine as any other
download: multiple connections, the global and per-download rate limits,
pause/resume, and live progress all behave normally. The file is named after
the media title.

Extraction is attempted only when the probe finds a web page (`text/html`), so
ordinary downloads never start the tool. Pages yt-dlp does not recognise are
downloaded as before.

### Separate video and audio streams

Most video sites publish anything above 1080p as a video-only stream plus an
audio-only stream. When there is no single-file format, Surge downloads both
and joins them with [`ffmpeg`](https://ffmpeg.org):

- both streams are fetched by the normal engine, one after the other, and
  count as **one** download with one queue slot, one progress bar and one file;
- the streams are joined by stream copy (`-c copy`), never re-encoded, so it
  costs a few seconds rather than minutes and loses no quality;
- the container follows the streams: mp4 + m4a gives `.mp4`, webm + webm gives
  `.webm`, and a mixed pair gives `.mkv`, which accepts either;
- the intermediate stream files live next to the final file as
  `<name>.p0.video.surge` / `<name>.p1.audio.surge` and are removed once the
  join succeeds. A failed join keeps them, so a retry does not re-download.

`ffmpeg` is required for this: without it such a page is refused with
`extractor not available: only separate video and audio streams are offered;
install ffmpeg to combine them` rather than downloading half a video. Override
the binary with `SURGE_FFMPEG`.

Pause and resume work at stream granularity: a finished stream is recorded and
not fetched again, while the stream that was in flight restarts. Completion is
recorded explicitly rather than inferred from the file's size, because the
concurrent downloader preallocates each stream file to its full length - and it
is only honoured while the stream's file is still on disk and at least as large
as the stream, so a working file that disappeared is downloaded again instead
of being muxed empty.

### Fragmented streams (HLS)

When a site offers neither a single file nor a downloadable pair, Surge falls
back to the HLS playlist, and a playlist URL pasted directly is treated the
same way:

```bash
surge add -- "https://example.com/video/master.m3u8"
```

Surge reads the playlist, picks the highest-quality variant of a master
playlist, downloads every fragment through the same connection pool as any
other download (so the proxy and both rate limits apply), concatenates them in
playlist order and remuxes the result with `ffmpeg` into the final container.
Fragments live in `<name>.frags/` next to the file while the download runs and
are removed once the assembly succeeds; a failed assembly keeps them, so a
retry does not re-download. Removing the download removes them too, along with
every other intermediate.

Pause and resume work at fragment granularity: a fragment is written to a
temporary name and renamed, so only a complete fragment counts and a resumed
stream fetches just what is missing. The directory records which playlist it
belongs to, so fragments left by a different stream are discarded rather than
assembled into the wrong file.

A playlist is remote content: it names its own fragment URLs, and can name any
host. Cookies and authorization headers obtained for the page are therefore
sent only to that page's host, a fragment answered with the wrong byte range
is rejected, and a playlist declaring more than 50,000 fragments is refused.

Three kinds of stream are refused rather than downloaded wrongly:

- **Live streams** (no `EXT-X-ENDLIST`) have no size and no end; recording one
  is a different feature from downloading a file.
- **Encrypted streams** (`EXT-X-KEY` with a method other than `NONE`) would be
  saved as ciphertext no player can open.
- **Streams whose audio is a separate rendition** (`EXT-X-MEDIA` with its own
  URI, or a variant with an `AUDIO` group) would give a silent video; joining
  renditions is not implemented.

DASH (`http_dash_segments`) is not supported. Fragment downloads also give up
two engine features that need byte ranges: there is no resume *inside* a
fragment, and the chunk map stays empty.

### What is still refused

- **Playlists and galleries.** Surge tracks one file per download, so add the
  items individually (or use `surge add url1 url2 ...`).
- **Media with no audio anywhere** — no progressive format, no audio stream to
  pair, no muxed playlist. Refused with
  `no single-file format available for this URL`; use yt-dlp directly.

Extracted media URLs are signed, short-lived and bound to your IP. Surge
stores the page URL alongside the download and re-resolves it when you resume,
so a download paused for hours still continues rather than failing with a 403.

Set `SURGE_YTDLP` if the binary lives somewhere unusual. With yt-dlp absent,
behaviour is exactly as before.

### Proxy

Media extraction honours the `proxy_url` network setting. Surge applies that
proxy to every request it makes itself through its shared transport, but
`yt-dlp` runs as a separate process, so the setting is passed to it explicitly
as `--proxy`. A proxied Surge therefore resolves media pages through the proxy
too, which matters for correctness and not only for privacy: the CDN URLs a
site hands back are signed for the address that asked for them, so resolving
directly and downloading through a proxy would be rejected.

With `proxy_url` empty, both Surge and yt-dlp fall back to the environment
(`http_proxy` / `https_proxy`), which is the same default the transport uses.

## Fonts

Surge bundles a Nerd Font, but terminal fonts are controlled by your terminal
emulator. Install the bundled font and set your terminal to
`JetBrainsMono Nerd Font Mono`.

See [FONTS.md](FONTS.md) for install steps and licensing details.
