#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/my-t-companion}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-my-t-companion}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/my-t-companion}"
ALPINE_IMAGE="${ALPINE_IMAGE:-alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc}"
INCLUDE_PAIRING=false

if [[ "${1:-}" == "--include-pairing" ]]; then
  INCLUDE_PAIRING=true
  shift
fi
[[ $# -eq 0 ]] || { printf 'Usage: sudo %s [--include-pairing]\n' "$0" >&2; exit 2; }
[[ "${EUID:-$(id -u)}" -eq 0 ]] || { printf 'Run with sudo or as root.\n' >&2; exit 1; }

for command_name in docker tar sha256sum mktemp; do
  command -v "$command_name" >/dev/null 2>&1 || { printf 'Missing: %s\n' "$command_name" >&2; exit 1; }
done

volume="${COMPOSE_PROJECT}_notification-state"
docker volume inspect "$volume" >/dev/null
mkdir -p "$BACKUP_DIR"
chmod 0700 "$BACKUP_DIR"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir "$work_dir/data"

docker run --rm -v "$volume:/source:ro" -v "$work_dir/data:/backup" "$ALPINE_IMAGE" \
  sh -c 'for f in parking-events.json software-notifications.json; do [ ! -f "/source/$f" ] || cp "/source/$f" /backup/; done'
if [[ "$INCLUDE_PAIRING" == true ]]; then
  docker run --rm -v "$volume:/source:ro" -v "$work_dir/data:/backup" "$ALPINE_IMAGE" \
    sh -c '[ ! -f /source/software-push-pairing.json ] || cp /source/software-push-pairing.json /backup/'
fi

version="$(tr -d '[:space:]' < "$INSTALL_DIR/VERSION" 2>/dev/null || printf unknown)"
{
  printf 'format_version=1\n'
  printf 'companion_version=%s\n' "$version"
  printf 'created_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'includes_pairing=%s\n' "$INCLUDE_PAIRING"
  printf 'scope=companion_durable_state_only\n'
} > "$work_dir/MANIFEST"

archive="$BACKUP_DIR/my-t-companion-${version}-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
tar -C "$work_dir" -czf "$archive" MANIFEST data
chmod 0600 "$archive"
(cd "$BACKUP_DIR" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
chmod 0600 "$archive.sha256"

# Keep the newest 12 manual/automatic archives so backups cannot grow forever.
mapfile -t old_archives < <(find "$BACKUP_DIR" -maxdepth 1 -type f -name 'my-t-companion-*.tar.gz' -printf '%T@ %p\n' | sort -rn | tail -n +13 | cut -d' ' -f2-)
for old_archive in "${old_archives[@]}"; do
  rm -f -- "$old_archive" "$old_archive.sha256"
done
printf '%s\n' "$archive"
