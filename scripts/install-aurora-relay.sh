#!/usr/bin/env bash

set -Eeuo pipefail

readonly binary_target=/usr/local/bin/aurora-relay
readonly unit_target=/etc/systemd/system/aurora-relay.service

replace=false

usage() {
  cat <<'EOF'
Usage: scripts/install-aurora-relay.sh [--replace]

Build and install Aurora Relay and its systemd unit.

  --replace   Allow replacement of an existing installed binary or unit.
  -h, --help  Show this help.

The script never enables, starts, stops, or restarts the service.
EOF
}

for argument in "$@"; do
  case "$argument" in
    --replace)
      replace=true
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'error: unknown argument: %s\n' "$argument" >&2
      usage >&2
      exit 2
      ;;
  esac
done

script_directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly repository_root=$(cd -- "$script_directory/.." && pwd)
readonly unit_source="$repository_root/deploy/systemd/aurora-relay.service"

for command in go install systemctl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'error: required command not found: %s\n' "$command" >&2
    exit 1
  fi
done

if [[ ! -f "$repository_root/go.mod" || ! -f "$unit_source" ]]; then
  printf 'error: repository files are incomplete under %s\n' "$repository_root" >&2
  exit 1
fi

privilege_command=()
if ((EUID != 0)); then
  if ! command -v sudo >/dev/null 2>&1; then
    printf 'error: sudo is required when not running as root\n' >&2
    exit 1
  fi
  privilege_command=(sudo)
fi

if [[ "$replace" != true ]] && { [[ -e "$binary_target" ]] || [[ -e "$unit_target" ]]; }; then
  printf 'error: an installation already exists; inspect it and rerun with --replace to overwrite it\n' >&2
  exit 1
fi

build_directory=$(mktemp -d "${TMPDIR:-/tmp}/aurora-relay-build.XXXXXXXX")
readonly build_directory
trap 'rm -rf -- "$build_directory"' EXIT

printf 'Building Aurora Relay...\n'
(
  cd -- "$repository_root"
  go build -trimpath -o "$build_directory/aurora-relay" ./cmd/aurora-relay
)

printf 'Installing %s...\n' "$binary_target"
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$build_directory/aurora-relay" "$binary_target"

printf 'Installing %s...\n' "$unit_target"
"${privilege_command[@]}" install -o root -g root -m 0644 \
  "$unit_source" "$unit_target"

printf 'Reloading systemd units...\n'
"${privilege_command[@]}" systemctl daemon-reload

printf 'Installation complete. The service was not enabled, started, stopped, or restarted.\n'
printf 'After stopping any manually running relay, enable and start separately:\n'
printf '  sudo systemctl enable aurora-relay.service\n'
printf '  sudo systemctl start aurora-relay.service\n'
printf 'After an upgrade, explicitly restart an already running service with:\n'
printf '  sudo systemctl restart aurora-relay.service\n'

printf 'Verify with:\n'
printf '  systemctl status aurora-relay.service\n'
printf '  journalctl -u aurora-relay.service\n'
printf '  curl -i http://127.0.0.1:8080/presence\n'
