# Linux desktop integration

Surge includes a Linux desktop integration built from three independent
pieces:

- a systemd user service that supervises the existing Surge headless server;
- reusable, environment-neutral Quickshell components with a compact status
  icon and a control panel;
- an Omarchy bar-widget plugin that draws the same client and data with
  Omarchy's own UI components and theme.

The widget shows server availability through an icon indicator, plus active
and paused counts, aggregate speed, per-download progress, and connection
errors in its panel. Each download can be started, stopped, or deleted through
Surge's existing CLI/API, and the supplied systemd user service can be started
or stopped separately.

The integration targets Wayland and does not use X11 APIs. The reusable
components require Quickshell 0.3.x. The Omarchy plugin follows the Omarchy
4.x shell plugin contract.

## Architecture

The desktop code does not implement another downloader or network protocol.
Its asynchronous Quickshell.Io.Process objects invoke:

    surge ls --json --server-only
    surge add [--output DIR] -- URL
    surge pause -- ID
    surge resume -- ID
    surge rm -- ID

The optional service controls invoke systemctl --user for surge.service.
Surge's CLI discovers the active local port and credential, then uses the
existing authenticated HTTP API. The --server-only flag is important for a
status widget: it makes a failed live connection an error instead of silently
showing Surge's offline database fallback.

All process execution uses argument arrays, not a shell command string, so
nothing is interpolated into a shell. The `--` terminator and the checks
behind it matter for the same reason at the argument level: the Surge CLI and
systemctl both parse interspersed flags, so a URL, a server-supplied download
id, or a `serviceUnit` beginning with `-` would otherwise be read as an
option. The widget therefore accepts only http/https URLs, ids matching
`[A-Za-z0-9._:-]`, an absolute `downloadDirectory`, and a `serviceUnit` that
looks like a unit name; anything else is refused before a process is spawned.

### Layout

    integrations/quickshell/surgedm/manifest.json   Omarchy plugin manifest
    integrations/quickshell/surgedm/Panel.qml       Omarchy bar-widget entry point
    integrations/quickshell/surgedm/Core/           Omarchy-free reusable pieces
    integrations/quickshell/shell.qml               standalone Quickshell example

`Panel.qml` is the plugin's only entry point. It is Omarchy-native: it imports
`qs.Ui` and takes its colors, spacing, and font from the active Omarchy theme
through `Style` and `Color`. Everything under `Core/` is free of Omarchy
imports, so a plain Quickshell configuration can use the same client, model,
and panel content.

## Install the systemd user service

Install Surge first and confirm that it is available:

    surge --version

From a Surge source checkout, install, enable, and start the unit:

    ./integrations/systemd/install.sh --enable

No sudo is used. The installer copies the unit to:

    $XDG_CONFIG_HOME/systemd/user/surge.service

or ~/.config/systemd/user/surge.service when XDG_CONFIG_HOME is unset.

The equivalent manual installation is:

    install -Dm644 integrations/systemd/surge.service \
      "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/surge.service"
    systemctl --user daemon-reload
    systemctl --user enable --now surge.service

Useful management commands:

    systemctl --user status surge.service
    systemctl --user restart surge.service
    systemctl --user disable --now surge.service
    journalctl --user -u surge.service -f

The unit runs `surge server --bind 127.0.0.1`, starts at the user's default
systemd target, restarts after crashes, and sends SIGTERM for Surge's existing
clean shutdown path. Loopback binding keeps this automatically started API off
the LAN; Bearer authentication is still required.

### Hardening

The unit runs the containment that is actually meaningful for an unprivileged
process parsing bytes off the network: a `@system-service @pkey` seccomp allow
list with `SystemCallArchitectures=native`, `ProtectProc=invisible` plus
`ProcSubset=pid`, `ProtectClock`, `ProtectHostname`, `ProtectKernelLogs`,
`ProtectKernelTunables`, `ProtectKernelModules`, `ProtectControlGroups`,
`ProtectSystem=full`, `RestrictNamespaces`, `RestrictRealtime`,
`RestrictSUIDSGID`, `RestrictAddressFamilies`, `PrivateDevices`,
`LockPersonality` and `NoNewPrivileges`. Measured with
`systemd-analyze --user security surge.service`, that moves the unit from
exposure 6.1 (MEDIUM) to 3.9 (OK).

`@pkey` is in the allow list because media extraction runs yt-dlp, which runs
a JavaScript engine (deno) to solve YouTube's "n" challenge, and V8 allocates
memory protection keys. `@system-service` alone blocks `pkey_alloc`
(syscall 330) and the child is killed with SIGSYS.

Four directives are deliberately absent, and re-adding them will break
things:

- `ProtectHome` — Surge downloads to user-selected destinations under $HOME.
- `PrivateTmp` — with it, a download whose destination is /tmp or /var/tmp
  lands in the service's private mount namespace. Surge reports the download
  as completed and the file is not where the user asked for it (verified: the
  file was only reachable through /proc/$MAINPID/root/tmp).
- `UMask` — it would change the mode of every downloaded file. On the usual
  0700 home directory, world-readable download files are already unreachable
  by other users.
- `MemoryDenyWriteExecute` — a JIT needs writable-executable memory. With it
  set, deno panics, yt-dlp reports "n challenge solving failed: Some formats
  may be missing", every progressive YouTube format disappears from its
  output, and media extraction fails with "no single-file format available".

Those four are why `systemd-analyze security` still reports a non-zero score;
the remaining points are those omissions plus `@privileged`/`@resources`
inside `@system-service`.

### Executable lookup

`install.sh` pins ExecStart to the absolute path that `command -v surge`
resolved at install time, so an auto-started network service does not depend
on PATH ordering; it prints the path it used. The unit as shipped in the
repository uses `/usr/bin/env surge` so that it is valid on its own.

If the binary later moves, or you install the unit by hand and the journal
reports that surge cannot be found, create a user override:

    systemctl --user edit surge.service

Add these lines, using the result of command -v surge:

    [Service]
    ExecStart=
    ExecStart=/absolute/path/to/surge server --bind 127.0.0.1

Then reload and restart:

    systemctl --user daemon-reload
    systemctl --user restart surge.service

## Authentication and first-time provisioning

No separate desktop token is created.

On its first start, surge server uses Surge's existing token provisioning:

1. If SURGE_TOKEN or --token is set, Surge uses and persists that value.
2. Otherwise Surge reuses the existing per-user token when present.
3. If no token exists, Surge generates one and writes it to
   $XDG_STATE_HOME/surge/token (normally ~/.local/state/surge/token) with
   mode 0600.
4. Surge publishes the active port and a per-session runtime copy of the token
   for local client discovery. Both the runtime directory and that copy are
   owner-only (0700/0600); Surge tightens the modes on startup, so a
   world-readable copy left by an older version is corrected.
5. The Quickshell widget invokes the Surge CLI. The CLI reads the credential
   and sends the existing Bearer-authenticated API request.

The QML source never contains, displays, copies, or directly reads the token.
The token exists only in Surge's protected state/runtime files and in the
short-lived Surge client process. It survives reboots through the state file.
Existing tokens and manual SURGE_TOKEN configuration remain valid.
The supplied service additionally binds to loopback. Users who deliberately
change `--bind` to a non-loopback address must protect network access and
should use a trusted HTTPS reverse proxy for remote clients.

For an explicit service token, create the optional environment file with
owner-only permissions:

    install -Dm600 /dev/null "$HOME/.config/surge/desktop.env"
    ${EDITOR:-vi} "$HOME/.config/surge/desktop.env"

Put a SURGE_TOKEN assignment in that file, then restart the service. Do not
commit or share desktop.env. Omitting it is recommended for normal local use
because automatic generation is already secure and requires no setup.

For a remote host, set the host setting. Providing SURGE_TOKEN in the
environment that starts Quickshell or the Omarchy shell is the simplest route
but the weakest: every Omarchy plugin shares one process and one environment,
including the stock bar's `custom` widget, which runs a shell.json-supplied
command through `bash -lc`. Anything the shell spawns, and anything that can
read /proc/<pid>/environ, then has the token. Prefer keeping the widget on the
local loopback server, or set `serviceControlsEnabled` to false and
authenticate the remote target from a terminal instead. Remote public hosts
should use HTTPS; Surge rejects insecure HTTP for public targets unless
explicitly allowed.

## General Quickshell setup

The reusable components live in integrations/quickshell/surgedm/Core.

To try the complete standalone example directly from the repository:

    quickshell --path integrations/quickshell

It creates a minimal top bar and is intended as a working demonstration. To
add Surge to an existing Quickshell bar, copy the component directory beside
your shell.qml:

    mkdir -p "$HOME/.config/quickshell/surgedm"
    cp -R integrations/quickshell/surgedm/Core \
      "$HOME/.config/quickshell/surgedm/"

Add the import near the top of shell.qml:

    import "surgedm/Core" as SurgeDM

Place this inside an existing PanelWindow or bar layout:

    SurgeDM.SurgeWidget {
      anchors.verticalCenter: parent.verticalCenter

      surgeCommand: "surge"
      pollInterval: 2000
      serviceUnit: "surge.service"
      serviceControlsEnabled: true
    }

The component sizes itself as a compact pill and carries its own explicit
color and font properties, because a plain Quickshell configuration has no
theme to read.

## Omarchy setup

Omarchy discovers a hand-installed plugin by directory name under
~/.config/omarchy/plugins. Copy this plugin's directory there, validate it,
let the shell pick it up, then enable and place it:

    plugin_dir="$HOME/.config/omarchy/plugins/io.github.surgedm.desktop"
    mkdir -p "$plugin_dir"
    cp -R integrations/quickshell/surgedm/. "$plugin_dir/"
    omarchy plugin validate "$plugin_dir"
    omarchy-shell shell rescanPlugins
    omarchy plugin enable io.github.surgedm.desktop --section right

`omarchy plugin validate` runs the same manifest checks the shell enforces, so
it fails before a broken plugin can land in the trusted plugins directory.
`rescanPlugins` is what makes a newly created directory known to the running
shell; `omarchy plugin enable` refuses an id the shell has not discovered yet.

The manifest declares `barWidget.defaultSection: "right"`, so `--section right`
above only makes the placement explicit. Move the widget later without
re-enabling it:

    omarchy bar move io.github.surgedm.desktop --section right

`omarchy bar move` also accepts `--index <n>`, `--before <id>`, and
`--after <id>`, and a section may be given positionally instead
(`omarchy bar move io.github.surgedm.desktop right`).

Confirm the plugin is known and enabled:

    omarchy plugin list

### Reloading

Two things reload on their own:

- saving any file under ~/.config/omarchy/plugins/ hot-reloads plugin code;
- saving ~/.config/omarchy/shell.json reloads the shell config.

`omarchy restart shell` is therefore not a routine step after editing the
plugin or its settings. One caveat observed on Omarchy 4.0.2: after a plugin
hot-reload the shell can keep the previous widget instance alive alongside
the new one (the log shows `Handler was registered but will not be used` for
the plugin's IPC target), and `omarchy-shell io.github.surgedm.desktop …`
calls keep going to the old instance. The bar itself shows the new code; if
the IPC surface looks stale, `omarchy restart shell` clears it.

### Developing against a checkout

A development checkout can be symlinked instead of copied:

    ln -s "$PWD/integrations/quickshell/surgedm" \
      "$HOME/.config/omarchy/plugins/io.github.surgedm.desktop"

Two caveats. `omarchy plugin validate` rejects symlinks found inside a plugin
folder, and a bare symlink path is itself such a link, so validate the target
with a trailing slash:

    omarchy plugin validate \
      "$HOME/.config/omarchy/plugins/io.github.surgedm.desktop/"

And the plugin watcher does not traverse symlinks, so edits inside the linked
checkout do not hot-reload. Push them with:

    omarchy-shell shell rescanPlugins

## Configuration

### Omarchy plugin settings

Omarchy keeps per-widget settings inline on the widget's own entry in
`bar.layout.<section>` of ~/.config/omarchy/shell.json. There is no
per-plugin settings file, no `config:` sub-object, and no deep merge: the
fields on the entry are exactly the values the plugin sees. The manifest's
`barWidget.defaults` and `barWidget.schema` are what a settings UI reads to
render and default the form.

Write a setting with `omarchy bar set <id> <key> <value> [--json]`. The value
is treated as a string unless `--json` is given, so use `--json` for numbers
and booleans only:

    omarchy bar set io.github.surgedm.desktop refreshIntervalSec 5 --json
    omarchy bar set io.github.surgedm.desktop serviceControlsEnabled false --json
    omarchy bar set io.github.surgedm.desktop host "127.0.0.1:1700"
    omarchy bar set io.github.surgedm.desktop downloadDirectory "$HOME/Downloads"

`omarchy bar set` edits the persisted layout through the running shell, so
enable the widget first.

| Key | Type | Default | Purpose |
| :-- | :-- | :-- | :-- |
| `command` | string | `surge` | Executable name on PATH, or an absolute path. |
| `host` | string | empty | host:port or URL; empty auto-detects the local server. |
| `refreshIntervalSec` | integer, 1–60 | `2` | Poll interval in **seconds**. |
| `downloadDirectory` | string | empty | Destination for downloads added from the panel. |
| `serviceUnit` | string | `surge.service` | systemd user unit the power switch controls. |
| `serviceControlsEnabled` | boolean | `true` | Show or hide the service power switch. |

The plugin takes its colors and font from the active Omarchy theme, not from
properties, so there are no theme settings to set.

### Generic SurgeWidget properties

Non-Omarchy Quickshell configurations set properties directly on
`Core/SurgeWidget.qml`. These are QML properties, not Omarchy settings keys.

| Property | Type | Default | Purpose |
| :-- | :-- | :-- | :-- |
| `surgeCommand` | string | `surge` | Executable name on PATH, or an absolute path. |
| `host` | string | empty | host:port or URL; empty auto-detects the local server. |
| `pollInterval` | int | `2000` | Poll interval in **milliseconds**; clamped to 500 minimum. |
| `downloadDirectory` | string | empty | Destination for downloads added from the panel. |
| `serviceUnit` | string | `surge.service` | systemd user unit the service controls act on. |
| `serviceControlsEnabled` | bool | `true` | Show or hide the service controls. |
| `popupAbove` | bool | `false` | Open the panel upward, for a bottom bar. |
| `backgroundColor` | color | `#17191f` | Pill and panel background. |
| `foregroundColor` | color | `#f3f4f6` | Primary text. |
| `accentColor` | color | `#63d8ff` | Progress and active highlights. |
| `urgentColor` | color | `#ff6b7a` | Errors and offline state. |
| `fontFamily` | string | `sans-serif` | Widget font family. |

Note the unit difference: the Omarchy plugin's `refreshIntervalSec` is in
seconds, the generic widget's `pollInterval` is in milliseconds. They are
separate settings; only the Omarchy plugin uses `refreshIntervalSec`.

When host is empty, the service may move to the next free port after 1700 and
the CLI/widget still discovers it. Avoid setting host unless a fixed target is
required.

## Interactions

Bar face:

| Widget | What it does | Interactions |
| :-- | :-- | :-- |
| `io.github.surgedm.desktop` | Surge status with active/paused counts and aggregate speed | left = open panel · right = start/stop the user service · middle = refresh now |

Panel keys:

| Key | Action |
| :-- | :-- |
| `Esc` | Close the panel. |
| `Tab` / `Shift+Tab` | Switch to the neighbouring bar panel. |
| `Up` / `Down`, `k` / `j` | Move the row cursor. |
| `Enter` / `Space` | With a row selected, start or stop it; otherwise focus the URL field. `Esc` in the field returns to the panel keys. |
| `x` | Delete the selected download. |
| `r` | Refresh now. |
| `s` | Toggle the user service. |

## Troubleshooting

### Widget says offline

Check the unit and live API:

    systemctl --user status surge.service
    journalctl --user -u surge.service -n 100 --no-pager
    surge ls --server-only --json

If the last command fails, the widget will fail for the same reason and show
a shortened version of that error.

### Authentication was rejected

Check whether a stale manual override is exported:

    systemctl --user show-environment | grep '^SURGE_TOKEN='

Do not print the token itself in bug reports. Remove an unintended override
from the user manager, then restart:

    systemctl --user unset-environment SURGE_TOKEN
    systemctl --user restart surge.service

If desktop.env contains an intentional token, keep that file mode 0600.

### Widget cannot find surge

Set the Omarchy `command` setting, or the generic widget's `surgeCommand`
property, to an absolute path. Use the systemd override in the executable
lookup section when the service has the same problem.

### Omarchy plugin is not listed

Validate the installed directory, then rescan:

    omarchy plugin validate \
      "$HOME/.config/omarchy/plugins/io.github.surgedm.desktop"
    omarchy-shell shell rescanPlugins
    omarchy plugin list

manifest.json and Panel.qml must be directly inside the plugin directory, not
nested under another surgedm directory. The directory name must be the
manifest id, io.github.surgedm.desktop.

### Omarchy plugin is listed but shows nothing

The plugin is discovered but not on the bar. Enable and place it:

    omarchy plugin enable io.github.surgedm.desktop --section right

## Uninstallation

Remove the Omarchy plugin. `omarchy plugin remove` disables it first, then
unlinks a symlinked checkout, deletes a git-managed clone, or moves a
hand-copied directory to a backup beside it, and rescans:

    omarchy plugin remove io.github.surgedm.desktop --yes

To disable it without uninstalling:

    omarchy plugin disable io.github.surgedm.desktop

Remove a general Quickshell copy and its SurgeWidget entry:

    rm -rf "$HOME/.config/quickshell/surgedm"

Remove the user service:

    systemctl --user disable --now surge.service
    rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/surge.service"
    rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/surge.service.d"
    systemctl --user daemon-reload

These commands deliberately leave Surge downloads, settings, database, and
authentication token intact. To remove that user data as well, first verify
that no other Surge installation uses it, then remove the relevant
~/.config/surge and ~/.local/state/surge directories.

## Current limitations

- Status is polled rather than held open on Surge's SSE stream. The period is
  the configurable `refreshIntervalSec` (default 2s) for the Omarchy plugin
  and `pollInterval` for the generic widget. This is simpler and resilient
  across shell reloads, but not instant.
- The compact panel exposes add, start, stop, and delete. Full history,
  rate-limit editing, and confirmation workflows remain in the TUI/CLI.
- The supplied unit is Linux/systemd-specific; the reusable Quickshell
  components can still monitor a manually started server.
