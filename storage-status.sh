#!/usr/bin/env bash
set -euo pipefail

COMPOSE_PROJECT="${COMPOSE_PROJECT:-my-t-companion}"
ALPINE_IMAGE="${ALPINE_IMAGE:-alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc}"
volume="${COMPOSE_PROJECT}_notification-state"
docker volume inspect "$volume" >/dev/null
printf 'My T Companion storage policy\n'
printf '  Parking events: long-term, newest 50,000 by default\n'
printf '  Software push deduplication: 180 days / 1,000 entries\n'
printf '  Charging push deduplication: 14 days / 2,000 entries\n'
printf '  Navigation push deduplication: 7 days / 2,000 entries\n'
printf '  Active charging/navigation snapshots: 48 hours / 12 hours\n'
printf '\nStored files (contents and secrets are never printed):\n'
docker run --rm -v "$volume:/data:ro" "$ALPINE_IMAGE" sh -c '
  for f in /data/*.json; do
    [ -f "$f" ] || continue
    printf "  %-38s %10s bytes\n" "$(basename "$f")" "$(wc -c < "$f")"
  done
  printf "  %-38s %10s bytes\n" "total" "$(du -sb /data | cut -f1)"
'
