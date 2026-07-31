#!/usr/bin/env bash

set -Eeuo pipefail

readonly hook_target=/usr/local/bin/aurora-claude-hook
readonly codex_hook_target=/usr/local/bin/aurora-codex-hook
readonly codex_notify_target=/usr/local/bin/aurora-codex-notify
readonly server_target=/usr/local/bin/aurora-presence-local-server
readonly wrapper_target=/usr/local/bin/aurora-claude-hook-local
readonly codex_wrapper_target=/usr/local/bin/aurora-codex-hook-local
readonly unit_target=/etc/systemd/system/aurora-presence-local-server.service

replace=false

usage() {
  cat <<'EOF'
Usage: scripts/install-aurora-presence-local-server.sh [--replace]

Build and install the Aurora local presence server, Claude and Codex hooks,
local hook wrappers, Codex notify adapter and systemd unit.

  --replace   Allow replacement of existing installed files.
  -h, --help  Show this help.

The script reloads systemd units but never enables, starts, stops or restarts
the service.
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
readonly unit_source="$repository_root/deploy/systemd/aurora-presence-local-server.service"
readonly wrapper_source="$repository_root/deploy/bin/aurora-claude-hook-local"
readonly codex_wrapper_source="$repository_root/deploy/bin/aurora-codex-hook-local"
readonly codex_notify_source="$repository_root/deploy/bin/aurora-codex-notify"

for command in go install systemctl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'error: required command not found: %s\n' "$command" >&2
    exit 1
  fi
done

if [[ ! -f "$repository_root/go.mod" ||
      ! -f "$unit_source" ||
      ! -f "$wrapper_source" ||
      ! -f "$codex_wrapper_source" ||
      ! -f "$codex_notify_source" ]]; then
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

targets=(
  "$hook_target"
  "$codex_hook_target"
  "$codex_notify_target"
  "$server_target"
  "$wrapper_target"
  "$codex_wrapper_target"
  "$unit_target"
)

if [[ "$replace" != true ]]; then
  for target in "${targets[@]}"; do
    if [[ -e "$target" ]]; then
      printf 'error: installation target already exists: %s\n' "$target" >&2
      printf 'inspect it and rerun with --replace to overwrite it\n' >&2
      exit 1
    fi
  done
fi

build_directory=$(mktemp -d "${TMPDIR:-/tmp}/aurora-presence-build.XXXXXXXX")
readonly build_directory
trap 'rm -rf -- "$build_directory"' EXIT

printf 'Building Aurora Claude hook...\n'
(
  cd -- "$repository_root"
  go build -trimpath \
    -o "$build_directory/aurora-claude-hook" \
    ./cmd/aurora-claude-hook
)

printf 'Building Aurora Codex hook...\n'
(
  cd -- "$repository_root"
  go build -trimpath \
    -o "$build_directory/aurora-codex-hook" \
    ./cmd/aurora-codex-hook
)

printf 'Building Aurora local presence server...\n'
(
  cd -- "$repository_root"
  go build -trimpath \
    -o "$build_directory/aurora-presence-local-server" \
    ./cmd/aurora-presence-local-server
)

printf 'Installing binaries and wrapper...\n'
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$build_directory/aurora-claude-hook" "$hook_target"
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$build_directory/aurora-codex-hook" "$codex_hook_target"
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$codex_notify_source" "$codex_notify_target"
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$build_directory/aurora-presence-local-server" "$server_target"
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$wrapper_source" "$wrapper_target"
"${privilege_command[@]}" install -o root -g root -m 0755 \
  "$codex_wrapper_source" "$codex_wrapper_target"

printf 'Installing systemd unit...\n'
"${privilege_command[@]}" install -o root -g root -m 0644 \
  "$unit_source" "$unit_target"

printf 'Reloading systemd units...\n'
"${privilege_command[@]}" systemctl daemon-reload

printf 'Installation complete. No service was enabled, started or restarted.\n'
printf 'Explicit activation commands:\n'
printf '  sudo systemctl enable aurora-presence-local-server.service\n'
printf '  sudo systemctl start aurora-presence-local-server.service\n'
