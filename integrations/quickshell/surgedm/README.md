# SurgeDM Quickshell integration

This directory is the Omarchy plugin. `manifest.json` plus `Panel.qml` are the
whole plugin: `Panel.qml` is the single bar-widget entry point and is
Omarchy-native, built from `qs.Ui` components and themed through `Style` and
`Color`.

Install it by copying this directory to:

    ~/.config/omarchy/plugins/io.github.surgedm.desktop

`Core/` holds the reusable pieces, free of Omarchy imports:

- `SurgeClient.qml` — asynchronous process client for the Surge CLI;
- `SurgeModel.js` — pure parsing and formatting, no QML dependencies;
- `SurgeWidget.qml` and `SurgePanelContent.qml` — a compact widget and panel
  for generic Quickshell bars.

A working standalone example is one directory up at `shell.qml`:

    quickshell --path integrations/quickshell

See docs/DESKTOP_INTEGRATION.md at the repository root for installation,
configuration, interactions, authentication, troubleshooting, security, and
removal instructions.
