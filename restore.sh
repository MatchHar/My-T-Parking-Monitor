#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/my-t-companion}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-my-t-companion}"
ALPINE_IMAGE="${ALPINE_IMAGE:-alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc}"
[[ "${EUID:-$(id -u)}" -eq 0 ]] || { printf 'Run with sudo or as root.\n' >&2; exit 1; }
[[ $# -eq 1 ]] || { printf 'Usage: sudo %s BACKUP.tar.gz\n' "$0" >&2; exit 2; }
archive="$(realpath "$1")"
[[ -f "$archive" ]] || { printf 'Backup not found.\n' >&2; exit 1; }

if [[ -f "$archive.sha256" ]]; then
  (cd "$(dirname "$archive")" && sha256sum --check "$(basename "$archive").sha256")
fi
if tar -tzf "$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
  printf 'Unsafe archive paths rejected.\n' >&2
  exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
tar -xzf "$archive" -C "$work_dir"
grep -qx 'format_version=1' "$work_dir/MANIFEST" || { printf 'Unsupported backup format.\n' >&2; exit 1; }
for restored_file in "$work_dir"/data/*.json; do
  [[ -e "$restored_file" ]] || continue
  [[ -f "$restored_file" && ! -L "$restored_file" ]] \
    || { printf 'Unsafe backup entry rejected.\n' >&2; exit 1; }
  case "$(basename "$restored_file")" in
    parking-events.json|software-notifications.json|software-push-pairing.json) ;;
    *) printf 'Unexpected backup file rejected.\n' >&2; exit 1 ;;
  esac
done

volume="${COMPOSE_PROJECT}_notification-state"
docker volume inspect "$volume" >/dev/null
"$INSTALL_DIR/backup.sh" >/dev/null
docker compose --project-name "$COMPOSE_PROJECT" --env-file "$INSTALL_DIR/.env" \
  --file "$INSTALL_DIR/docker-compose.yml" stop companion
trap 'docker compose --project-name "$COMPOSE_PROJECT" --env-file "$INSTALL_DIR/.env" --file "$INSTALL_DIR/docker-compose.yml" start companion >/dev/null 2>&1 || true; rm -rf "$work_dir"' EXIT

docker run --rm -v "$volume:/data" -v "$work_dir/data:/restore:ro" "$ALPINE_IMAGE" sh -c '
  for f in parking-events.json software-notifications.json software-push-pairing.json; do
    [ ! -f "/restore/$f" ] || { cp "/restore/$f" "/data/$f"; chown 10001:10001 "/data/$f"; chmod 0600 "/data/$f"; }
  done
'
docker compose --project-name "$COMPOSE_PROJECT" --env-file "$INSTALL_DIR/.env" \
  --file "$INSTALL_DIR/docker-compose.yml" start companion
printf 'Companion durable state restored. TeslaMate history and transient Live Activity state were not changed.\n'
