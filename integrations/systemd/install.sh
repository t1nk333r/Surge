#!/bin/sh
set -eu

usage() {
  printf '%s\n' "Usage: $0 [--enable]"
  printf '%s\n' "Install the SurgeDM systemd user unit; optionally enable and start it."
}

enable_now=false
case "${1:-}" in
  "")
    ;;
  --enable)
    enable_now=true
    ;;
  -h|--help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

if ! command -v systemctl >/dev/null 2>&1; then
  printf '%s\n' "systemctl was not found; this installer requires systemd." >&2
  exit 1
fi

if ! command -v surge >/dev/null 2>&1; then
  printf '%s\n' "surge was not found on PATH. Install SurgeDM first." >&2
  exit 1
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
config_home=${XDG_CONFIG_HOME:-"$HOME/.config"}
unit_dir="$config_home/systemd/user"
unit_path="$unit_dir/surge.service"

install -d -m 0755 "$unit_dir"
install -m 0644 "$script_dir/surge.service" "$unit_path"
systemctl --user daemon-reload

if [ "$enable_now" = true ]; then
  systemctl --user enable --now surge.service
  printf '%s\n' "Installed, enabled, and started surge.service."
else
  printf '%s\n' "Installed surge.service."
  printf '%s\n' "Enable it with: systemctl --user enable --now surge.service"
fi
