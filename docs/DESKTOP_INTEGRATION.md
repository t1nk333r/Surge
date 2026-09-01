# Linux desktop integration

Surge includes a Linux desktop integration built from three independent
pieces:

- a systemd user service that supervises the existing Surge headless server;
- a reusable Quickshell widget with a compact status icon and control panel;
- an Omarchy bar-widget adapter around the same Quickshell client and UI.

The widget shows server availability through an icon indicator, plus active
and paused counts, aggregate speed, per-download progress, and connection
errors in its panel. Each download can be started, stopped, or deleted through
Surge's existing CLI/API, and the supplied systemd user service can be started
or stopped separately.

The integration targets Wayland and does not use X11 APIs. The reusable
components require Quickshell 0.3.x. The Omarchy adapter follows the Omarchy
4.x shell plugin contract.

## Architecture

The desktop code does not implement another downloader or network protocol.
Its asynchronous Quickshell.Io.Process objects invoke:

    surge ls --json --server-only
    surge add URL
    surge pause ID
    surge resume ID
    surge rm ID

The optional Start and Stop buttons invoke systemctl --user for surge.service.
Surge's CLI discovers the active local port and credential, then uses the
existing authenticated HTTP API. The --server-only flag is important for a
status widget: it makes a failed live connection an error instead of silently
showing Surge's offline database fallback.

All process execution uses argument arrays, not a shell command string.
Polling and actions therefore remain asynchronous and avoid shell interpolation.

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
the LAN; Bearer authentication is still required. The unit also enables
conservative hardening while leaving the user's home directory writable because
Surge supports arbitrary user-selected download destinations.

### Executable lookup

The unit uses /usr/bin/env to find surge on the user manager's PATH. Normal
distribution, Homebrew, and ~/.local/bin installations are typically visible.
If the journal reports that surge cannot be found, create a user override:

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
4. Surge publishes the active port and a per-session runtime copy under the
   user's private XDG runtime directory for local client discovery.
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

For a remote host, set the widget's host property and provide SURGE_TOKEN in
the environment that starts Quickshell. Remote public hosts should use HTTPS;
Surge rejects insecure HTTP for public targets unless explicitly allowed.

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

The component sizes itself as a compact pill. Left-click opens its panel and
middle-click refreshes immediately.

## Omarchy setup

Install the plugin from a source checkout:

    plugin_dir="$HOME/.config/omarchy/plugins/io.github.surgedm.desktop"
    mkdir -p "$plugin_dir"
    cp -R integrations/quickshell/surgedm/. "$plugin_dir/"
    omarchy plugin validate "$plugin_dir"
    omarchy plugin enable io.github.surgedm.desktop --section right

Omarchy normally reloads user plugins and shell.json changes automatically.
If the widget does not appear:

    omarchy restart shell

The plugin is an Omarchy adapter only; Core remains free of Omarchy imports
and can be reused by any Quickshell configuration.

## Configuration

General Quickshell users set properties directly on SurgeWidget. Omarchy users
can store the same values on the widget entry:

    omarchy bar set io.github.surgedm.desktop pollInterval 3000 --json
    omarchy bar set io.github.surgedm.desktop downloadDirectory "$HOME/Downloads"
    omarchy bar set io.github.surgedm.desktop host "127.0.0.1:1700"

Available settings:

| Setting | Default | Purpose |
| :-- | :-- | :-- |
| command / surgeCommand | surge | Executable name or absolute path. |
| host | empty | Auto-detect local server; otherwise use host:port or URL. |
| pollInterval | 2000 | Poll period in milliseconds; values below 500 are clamped. |
| downloadDirectory | empty | Optional destination for downloads added in the panel. |
| serviceUnit | surge.service | User unit controlled by Start and Stop. |
| serviceControlsEnabled | true | Hide systemd controls for manually managed servers. |
| popupAbove | false | General widget only: open upward for a bottom bar. |
| backgroundColor, foregroundColor, accentColor, urgentColor | dark defaults | General widget theme colors. |
| fontFamily | sans-serif | General widget font. |

When host is empty, the service may move to the next free port after 1700 and
the CLI/widget still discovers it. Avoid setting host unless a fixed target is
required.

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

Set surgeCommand/command to an absolute path, and use the systemd override in
the executable lookup section when the service has the same problem.

### Omarchy plugin is not listed

Validate the copied directory and rescan:

    omarchy plugin validate \
      "$HOME/.config/omarchy/plugins/io.github.surgedm.desktop"
    omarchy-shell shell rescanPlugins

The manifest and BarWidget.qml must be directly inside the plugin directory,
not nested under another surgedm directory.

## Uninstallation

Remove the Omarchy plugin:

    omarchy plugin disable io.github.surgedm.desktop
    rm -rf "$HOME/.config/omarchy/plugins/io.github.surgedm.desktop"

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

- Status uses a two-second poll rather than holding Surge's SSE stream open.
  This is simpler and resilient across Quickshell reloads but not instant.
- The compact panel exposes add, start, stop, and delete. Full history,
  rate-limit editing, and confirmation workflows remain in the TUI/CLI.
- The supplied unit is Linux/systemd-specific; the reusable Quickshell
  components can still monitor a manually started server.
