#!/usr/bin/env bash
set -euo pipefail

TESLAMATE_DIR="${TESLAMATE_DIR:-/opt/teslamate}"
INSTALL_DIR="${INSTALL_DIR:-/opt/my-t-companion}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-my-t-companion}"
SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION_FILE="$SOURCE_DIR/VERSION"
[[ -f "$VERSION_FILE" ]] || {
  printf '[My T Companion] ERROR: VERSION file is missing.\n' >&2
  exit 1
}
VERSION="$(tr -d '[:space:]' < "$VERSION_FILE")"
ENV_FILE="$INSTALL_DIR/.env"
COMPOSE_FILE="$INSTALL_DIR/docker-compose.yml"
CADDY_FILE="${CADDY_FILE:-/etc/caddy/Caddyfile}"
MY_T_BASE_URL="${MY_T_BASE_URL:-}"
MY_T_AUTH_HEADER="${MY_T_AUTH_HEADER:-}"
MY_T_AUTH_PROBE_URL="${MY_T_AUTH_PROBE_URL:-}"
PUSH_INSTALLATION_ID="${PUSH_INSTALLATION_ID:-}"
PUSH_RELAY_URL="${PUSH_RELAY_URL:-}"
PUSH_RELAY_SECRET="${PUSH_RELAY_SECRET:-}"

log() {
  printf '[My T Companion] %s\n' "$*"
}

fail() {
  printf '[My T Companion] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Missing required command: $1"
}

read_env_value() {
  local key="$1"
  local file="$2"
  local line
  [[ -f "$file" ]] || return 1
  line="$(grep -E "^${key}=" "$file" 2>/dev/null | tail -n 1 || true)"
  [[ -n "$line" ]] || return 1
  printf '%s' "${line#*=}" | sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//"
}

# Strip quotes from a KEY=value line value.
strip_env_val() {
  printf '%s' "$1" | sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//"
}

# Read KEY from a running container's Config.Env.
container_env() {
  local container="$1"
  local key="$2"
  local line
  [[ -n "$container" ]] || return 1
  line="$(
    docker inspect "$container" \
      --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
      | grep -E "^${key}=" | tail -n 1 || true
  )"
  [[ -n "$line" ]] || return 1
  strip_env_val "${line#*=}"
}

# Parse KEY from `docker compose config` YAML-ish environment blocks (best-effort).
compose_config_env() {
  local key="$1"
  local cfg="$2"
  local line
  line="$(
    printf '%s\n' "$cfg" \
      | grep -E "^[[:space:]]+${key}:[[:space:]]" \
      | head -n 1 \
      | sed -E "s/^[[:space:]]+${key}:[[:space:]]*//" \
      || true
  )"
  [[ -n "$line" ]] || return 1
  strip_env_val "$line"
}

# Resolve config in order: existing shell env → compose config → container → .env → companion .env.
resolve_secret() {
  local key="$1"
  local alt_keys="${2:-}"
  local val=""
  local k
  local container

  # 1) Already exported (HostBox / operator overrides)
  for k in $key $alt_keys; do
    val="$(eval "printf '%s' \"\${$k-}\"")"
    if [[ -n "$val" ]]; then
      printf '%s' "$val"
      return 0
    fi
  done

  # 2) docker compose config (interpolated compose + env_file)
  if [[ -n "${COMPOSE_CONFIG_CACHE:-}" ]]; then
    for k in $key $alt_keys; do
      val="$(compose_config_env "$k" "$COMPOSE_CONFIG_CACHE" || true)"
      if [[ -n "$val" && "$val" != "null" ]]; then
        printf '%s' "$val"
        return 0
      fi
    done
  fi

  # 3) Running containers (database / teslamateapi / teslamate)
  for container in \
    "${DATABASE_CONTAINER:-}" \
    "${API_CONTAINER:-}" \
    "${TESLAMATE_CONTAINER:-}"; do
    [[ -n "$container" ]] || continue
    for k in $key $alt_keys; do
      val="$(container_env "$container" "$k" || true)"
      if [[ -n "$val" ]]; then
        printf '%s' "$val"
        return 0
      fi
    done
  done

  # 4) TeslaMate .env (optional — many installs only use compose environment:)
  if [[ -f "$TESLAMATE_DIR/.env" ]]; then
    for k in $key $alt_keys; do
      val="$(read_env_value "$k" "$TESLAMATE_DIR/.env" || true)"
      if [[ -n "$val" ]]; then
        printf '%s' "$val"
        return 0
      fi
    done
  fi

  # 5) Prior companion install
  if [[ -f "$ENV_FILE" ]]; then
    for k in $key $alt_keys; do
      val="$(read_env_value "$k" "$ENV_FILE" || true)"
      if [[ -n "$val" ]]; then
        printf '%s' "$val"
        return 0
      fi
    done
  fi

  return 1
}

find_compose_service_id() {
  local svc
  (
    cd "$TESLAMATE_DIR"
    for svc in "$@"; do
      id="$(docker compose ps -q "$svc" 2>/dev/null || true)"
      if [[ -n "$id" ]]; then
        printf '%s' "$id"
        return 0
      fi
    done
    return 1
  )
}

# Prefer a Docker network shared with TeslaMate / API, not an unrelated side network.
pick_shared_network() {
  local db_id="$1"
  local other_ids="$2"
  local nets
  local n
  local oid
  nets="$(
    docker inspect "$db_id" \
      --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}' 2>/dev/null \
      || true
  )"
  [[ -n "$nets" ]] || return 1
  while IFS= read -r n; do
    [[ -n "$n" ]] || continue
    for oid in $other_ids; do
      [[ -n "$oid" ]] || continue
      if docker inspect "$oid" \
        --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}' 2>/dev/null \
        | grep -qx "$n"; then
        printf '%s' "$n"
        return 0
      fi
    done
  done <<< "$nets"
  # Fallback: first network on the database container
  printf '%s' "$(printf '%s\n' "$nets" | head -n 1)"
}

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  fail "Run with sudo or as root."
fi

require_command docker
require_command openssl
require_command sed
require_command awk
require_command curl

[[ -d "$TESLAMATE_DIR" ]] || fail "TeslaMate directory not found: $TESLAMATE_DIR"
[[ -f "$TESLAMATE_DIR/docker-compose.yml" || -f "$TESLAMATE_DIR/compose.yml" || -f "$TESLAMATE_DIR/compose.yaml" ]] \
  || fail "TeslaMate docker-compose.yml (or compose.yml) not found in $TESLAMATE_DIR."
# .env is optional: many deployments put secrets only in compose environment: blocks.
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
  || fail "Invalid or missing VERSION file."
[[ -f "$SOURCE_DIR/Dockerfile" && -f "$SOURCE_DIR/main.go" &&
   -f "$SOURCE_DIR/notification.go" && -f "$SOURCE_DIR/charging_notification.go" &&
   -f "$SOURCE_DIR/navigation_notification.go" && -f "$SOURCE_DIR/parking_event_monitor.go" ]] \
  || fail "Run install.sh from a complete My-T-Companion checkout."

log "Checking the existing TeslaMate deployment"
DATABASE_CONTAINER="$(
  find_compose_service_id database db postgres teslamate-db teslamate_db || true
)"
if [[ -z "$DATABASE_CONTAINER" ]]; then
  # Last resort: running postgres-like container
  DATABASE_CONTAINER="$(
    docker ps --format '{{.ID}} {{.Names}} {{.Image}}' 2>/dev/null \
      | awk 'tolower($0) ~ /postgres|teslamate.*db|\/db$/ { print $1; exit }' || true
  )"
fi
[[ -n "$DATABASE_CONTAINER" ]] || fail "TeslaMate database container is not running (tried service names: database, db, postgres, …)."

API_CONTAINER="$(
  find_compose_service_id teslamateapi api teslamate-api || true
)"
TESLAMATE_CONTAINER="$(
  find_compose_service_id teslamate || true
)"
MQTT_CONTAINER="$(
  find_compose_service_id mosquitto mqtt eclipse-mosquitto broker || true
)"

COMPOSE_CONFIG_CACHE="$(
  cd "$TESLAMATE_DIR"
  docker compose config 2>/dev/null || true
)"

# Prefer a network shared with API / TeslaMate / MQTT when those containers exist.
database_network="$(
  pick_shared_network "$DATABASE_CONTAINER" "${API_CONTAINER:-} ${TESLAMATE_CONTAINER:-} ${MQTT_CONTAINER:-}" || true
)"
[[ -n "$database_network" ]] || fail "Unable to detect the TeslaMate Docker network."
log "Using Docker network: $database_network"

database_pass="$(
  resolve_secret DATABASE_PASS "TM_DB_PASS POSTGRES_PASSWORD POSTGRES_PASS" || true
)"
[[ -n "$database_pass" ]] || fail "DATABASE_PASS not found. Sources tried: shell env, docker compose config, running containers, $TESLAMATE_DIR/.env. Export DATABASE_PASS=… and re-run."

database_user="$(
  resolve_secret DATABASE_USER "POSTGRES_USER TM_DB_USER" || printf 'teslamate'
)"
database_name="$(
  resolve_secret DATABASE_NAME "POSTGRES_DB TM_DB_NAME" || printf 'teslamate'
)"
database_host="$(
  resolve_secret DATABASE_HOST "POSTGRES_HOST" || true
)"
if [[ -z "$database_host" ]]; then
  # Prefer compose service name that answered for the DB container
  database_host="$(
    docker inspect "$DATABASE_CONTAINER" --format '{{index .Config.Labels "com.docker.compose.service"}}' 2>/dev/null || true
  )"
fi
[[ -n "$database_host" ]] || database_host="database"

mqtt_broker_url="$(
  resolve_secret MQTT_BROKER_URL "MQTT_URL" || true
)"
mqtt_needs_host_gateway=false
if [[ -z "$mqtt_broker_url" ]]; then
  mqtt_host="$(resolve_secret MQTT_HOST "" || true)"
  mqtt_port="$(resolve_secret MQTT_PORT "" || printf '1883')"
  if [[ -n "$mqtt_host" ]]; then
    mqtt_broker_url="tcp://${mqtt_host}:${mqtt_port}"
    # host-gateway / bare IP / localhost → Docker needs host.docker.internal mapping
    if [[ "$mqtt_host" == "host.docker.internal" || "$mqtt_host" == "localhost" || "$mqtt_host" == "127.0.0.1" ]]; then
      mqtt_needs_host_gateway=true
      mqtt_broker_url="tcp://host.docker.internal:${mqtt_port}"
    fi
  elif [[ -n "$MQTT_CONTAINER" ]]; then
    mqtt_svc="$(
      docker inspect "$MQTT_CONTAINER" --format '{{index .Config.Labels "com.docker.compose.service"}}' 2>/dev/null || true
    )"
    [[ -n "$mqtt_svc" ]] || mqtt_svc="mosquitto"
    mqtt_broker_url="tcp://${mqtt_svc}:1883"
    log "MQTT: docker service '$mqtt_svc' on TeslaMate network"
  elif ss -lntp 2>/dev/null | grep -qE ':1883\b' || netstat -lntp 2>/dev/null | grep -qE ':1883\b'; then
    # HostBox and many VPS: system mosquitto, not a compose service
    mqtt_broker_url="tcp://host.docker.internal:1883"
    mqtt_needs_host_gateway=true
    log "MQTT: host listener :1883 → host.docker.internal (system broker)"
  else
    mqtt_broker_url="tcp://mosquitto:1883"
    log "MQTT: default tcp://mosquitto:1883 (ensure broker is reachable on the stack network)"
  fi
elif [[ "$mqtt_broker_url" == *host.docker.internal* || "$mqtt_broker_url" == *127.0.0.1* || "$mqtt_broker_url" == *localhost* ]]; then
  mqtt_needs_host_gateway=true
  mqtt_broker_url="${mqtt_broker_url//127.0.0.1/host.docker.internal}"
  mqtt_broker_url="${mqtt_broker_url//localhost/host.docker.internal}"
fi

api_token="$(
  resolve_secret MY_T_API_TOKEN "TM_API_TOKEN API_TOKEN" || true
)"
generated_token=false
if [[ -z "$api_token" ]]; then
  api_token="$(openssl rand -hex 32)"
  generated_token=true
fi

timezone="$(
  resolve_secret TZ "" || printf 'UTC'
)"

# TeslaMate app version for My T (avoids HTML scrape of LiveView / :4000 / Access).
teslamate_version="$(
  resolve_secret TESLAMATE_VERSION "" || true
)"
if [[ -z "$teslamate_version" && -n "${TESLAMATE_CONTAINER:-}" ]]; then
  tm_image="$(docker inspect "$TESLAMATE_CONTAINER" --format '{{.Config.Image}}' 2>/dev/null || true)"
  if [[ -n "$tm_image" && "$tm_image" == *:* ]]; then
    teslamate_version="${tm_image##*:}"
    # Drop digests / latest noise for display
    if [[ "$teslamate_version" == "latest" || "$teslamate_version" == sha256* ]]; then
      teslamate_version=""
    fi
  fi
fi

log "Config source: .env is optional; used compose config / containers / env when present."
log "DB host=${database_host} user=${database_user} name=${database_name}; MQTT=${mqtt_broker_url}"
if [[ -n "$teslamate_version" ]]; then
  log "Detected TeslaMate image tag for App version display: $teslamate_version"
fi

push_installation_id="${PUSH_INSTALLATION_ID:-$(read_env_value PUSH_INSTALLATION_ID "$ENV_FILE" || true)}"
push_relay_url="${PUSH_RELAY_URL:-$(read_env_value PUSH_RELAY_URL "$ENV_FILE" || true)}"
push_relay_secret="${PUSH_RELAY_SECRET:-$(read_env_value PUSH_RELAY_SECRET "$ENV_FILE" || true)}"
if [[ -n "$push_relay_url" && ! "$push_relay_url" =~ ^https:// ]]; then
  fail "PUSH_RELAY_URL must use HTTPS."
fi
push_values=0
[[ -n "$push_installation_id" ]] && push_values=$((push_values + 1))
[[ -n "$push_relay_url" ]] && push_values=$((push_values + 1))
[[ -n "$push_relay_secret" ]] && push_values=$((push_values + 1))
if [[ "$push_values" -ne 0 && "$push_values" -ne 3 ]]; then
  fail "Software push requires PUSH_INSTALLATION_ID, PUSH_RELAY_URL, and PUSH_RELAY_SECRET together."
fi

auth_probe_url="$(
  if [[ -n "$MY_T_AUTH_PROBE_URL" ]]; then
    printf '%s' "$MY_T_AUTH_PROBE_URL"
  else
    read_env_value AUTH_PROBE_URL "$ENV_FILE" || true
  fi
)"
if [[ -f "$CADDY_FILE" ]]; then
  api_hostname="$(
    awk '/^[[:space:]]*@api_host[[:space:]]+host[[:space:]]+/ {gsub(/"/, "", $3); print $3; exit}' "$CADDY_FILE"
  )"
  if [[ -z "$auth_probe_url" && -n "$api_hostname" ]]; then
    auth_probe_url="https://${api_hostname}/api/ping"
  fi
fi

if [[ "$generated_token" == true && -z "$auth_probe_url" ]]; then
  fail "No reusable TeslaMate API authentication was detected. Export MY_T_API_TOKEN=… (or API_TOKEN), put it in compose environment, or set MY_T_AUTH_PROBE_URL for the protected /api/ping endpoint. A TeslaMate .env file is optional."
fi

if [[ -n "$auth_probe_url" ]]; then
  unauthenticated_status="$(
    curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
      "$auth_probe_url" || true
  )"
  if [[ "$unauthenticated_status" =~ ^2 ]]; then
    fail "The existing API authentication probe is publicly accessible. Protect /api/ping before installing the companion."
  fi
fi

log "Installing service files in $INSTALL_DIR"
install -d -m 0755 "$INSTALL_DIR"
if [[ "$SOURCE_DIR" != "$INSTALL_DIR" ]]; then
  install -m 0644 "$SOURCE_DIR/Dockerfile" "$INSTALL_DIR/Dockerfile"
  install -m 0644 "$SOURCE_DIR/go.mod" "$INSTALL_DIR/go.mod"
  if [[ -f "$SOURCE_DIR/go.sum" ]]; then
    install -m 0644 "$SOURCE_DIR/go.sum" "$INSTALL_DIR/go.sum"
  fi
  install -m 0644 "$SOURCE_DIR/main.go" "$INSTALL_DIR/main.go"
  install -m 0644 "$SOURCE_DIR/notification.go" "$INSTALL_DIR/notification.go"
  install -m 0644 "$SOURCE_DIR/charging_notification.go" "$INSTALL_DIR/charging_notification.go"
  install -m 0644 "$SOURCE_DIR/navigation_notification.go" "$INSTALL_DIR/navigation_notification.go"
  install -m 0644 "$SOURCE_DIR/parking_event_monitor.go" "$INSTALL_DIR/parking_event_monitor.go"
  install -m 0644 "$SOURCE_DIR/storage_policy.go" "$INSTALL_DIR/storage_policy.go"
  install -m 0644 "$SOURCE_DIR/VERSION" "$INSTALL_DIR/VERSION"
  install -m 0755 "$SOURCE_DIR/install.sh" "$INSTALL_DIR/install.sh"
  install -m 0755 "$SOURCE_DIR/update.sh" "$INSTALL_DIR/update.sh"
  if [[ -f "$SOURCE_DIR/uninstall.sh" ]]; then
    install -m 0755 "$SOURCE_DIR/uninstall.sh" "$INSTALL_DIR/uninstall.sh"
  fi
  if [[ -f "$SOURCE_DIR/backup.sh" ]]; then
    install -m 0755 "$SOURCE_DIR/backup.sh" "$INSTALL_DIR/backup.sh"
  fi
  if [[ -f "$SOURCE_DIR/restore.sh" ]]; then
    install -m 0755 "$SOURCE_DIR/restore.sh" "$INSTALL_DIR/restore.sh"
  fi
  if [[ -f "$SOURCE_DIR/storage-status.sh" ]]; then
    install -m 0755 "$SOURCE_DIR/storage-status.sh" "$INSTALL_DIR/storage-status.sh"
  fi
  if [[ -f "$SOURCE_DIR/scripts/myt-doctor.sh" ]]; then
    install -m 0755 "$SOURCE_DIR/scripts/myt-doctor.sh" "$INSTALL_DIR/myt-doctor.sh"
  fi
else
  log "Running from the installed directory; service source files are already current"
fi
# Always refresh doctor when present in source (also on in-place reinstall).
if [[ -f "$SOURCE_DIR/scripts/myt-doctor.sh" ]]; then
  install -m 0755 "$SOURCE_DIR/scripts/myt-doctor.sh" "$INSTALL_DIR/myt-doctor.sh"
elif [[ -f "$SOURCE_DIR/myt-doctor.sh" ]]; then
  install -m 0755 "$SOURCE_DIR/myt-doctor.sh" "$INSTALL_DIR/myt-doctor.sh"
fi

umask 077
{
  printf 'DATABASE_PASS=%s\n' "$database_pass"
  printf 'DATABASE_USER=%s\n' "$database_user"
  printf 'DATABASE_NAME=%s\n' "$database_name"
  printf 'DATABASE_HOST=%s\n' "$database_host"
  printf 'MQTT_BROKER_URL=%s\n' "$mqtt_broker_url"
  printf 'MY_T_API_TOKEN=%s\n' "$api_token"
  printf 'AUTH_PROBE_URL=%s\n' "$auth_probe_url"
  printf 'TZ=%s\n' "$timezone"
  printf 'TESLAMATE_NETWORK=%s\n' "$database_network"
  printf 'TESLAMATE_VERSION=%s\n' "$teslamate_version"
  printf 'PUSH_INSTALLATION_ID=%s\n' "$push_installation_id"
  printf 'PUSH_RELAY_URL=%s\n' "$push_relay_url"
  printf 'PUSH_RELAY_SECRET=%s\n' "$push_relay_secret"
} > "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# Escape values for YAML double-quoted strings
yaml_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

# When MQTT is on the host (HostBox system mosquitto), Docker needs host-gateway.
extra_hosts_yaml=""
if [[ "$mqtt_needs_host_gateway" == true ]]; then
  extra_hosts_yaml="$(cat <<'EH'
    extra_hosts:
      - "host.docker.internal:host-gateway"
EH
)"
fi

cat > "$COMPOSE_FILE" <<YAML
services:
  companion:
    build: .
    image: myt/companion:${VERSION}
    restart: unless-stopped
    init: true
    cpus: 1.0
    mem_limit: 256m
    pids_limit: 128
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp:size=16m,mode=1777
${extra_hosts_yaml}    environment:
      DATABASE_USER: "$(yaml_escape "$database_user")"
      DATABASE_PASS: \${DATABASE_PASS}
      DATABASE_NAME: "$(yaml_escape "$database_name")"
      DATABASE_HOST: "$(yaml_escape "$database_host")"
      PGOPTIONS: -c default_transaction_read_only=on
      API_TOKEN: \${MY_T_API_TOKEN}
      AUTH_PROBE_URL: \${AUTH_PROBE_URL:-}
      TZ: \${TZ:-UTC}
      MQTT_BROKER_URL: "$(yaml_escape "$mqtt_broker_url")"
      TESLAMATE_VERSION: \${TESLAMATE_VERSION:-}
      PUSH_INSTALLATION_ID: \${PUSH_INSTALLATION_ID:-}
      PUSH_RELAY_URL: \${PUSH_RELAY_URL:-}
      PUSH_RELAY_SECRET: \${PUSH_RELAY_SECRET:-}
      PUSH_STATE_PATH: /data/software-notifications.json
      PARKING_EVENT_STATE_PATH: /data/parking-events.json
      PARKING_EVENT_RETENTION_DAYS: \${PARKING_EVENT_RETENTION_DAYS:-0}
      PARKING_EVENT_MAX_EVENTS: \${PARKING_EVENT_MAX_EVENTS:-50000}
    ports:
      - "127.0.0.1:8083:8080"
    volumes:
      - notification-state:/data
    healthcheck:
      test: ["CMD", "/app/states-api", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    networks:
      - teslamate

volumes:
  notification-state:

networks:
  teslamate:
    external: true
    name: \${TESLAMATE_NETWORK}
YAML

log "Building and starting the read-only monitor"
docker compose \
  --project-name "$COMPOSE_PROJECT" \
  --env-file "$ENV_FILE" \
  --file "$COMPOSE_FILE" \
  up -d --build

for _ in $(seq 1 20); do
  if curl --fail --silent --show-error http://127.0.0.1:8083/api/healthz >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail --silent --show-error http://127.0.0.1:8083/api/healthz >/dev/null \
  || fail "The monitor did not become healthy."

proxy_ready=false
if [[ -f "$CADDY_FILE" ]] && command -v caddy >/dev/null 2>&1; then
  missing_states=true
  missing_parking_events=true
  missing_capabilities=true
  missing_current_drive=true
  missing_push_history=true
  # Broad notifications/* covers software-update + Live Activity status routes
  missing_notifications=true
  grep -qE 'cars/.*/states|parking_states|my_t_parking_states' "$CADDY_FILE" && missing_states=false
  grep -qE 'parking-events|my_t_parking_events' "$CADDY_FILE" && missing_parking_events=false
  grep -qE 'api/v1/capabilities|parking_capabilities|my_t_parking_capabilities' "$CADDY_FILE" && missing_capabilities=false
  grep -qE 'current-drive|my_t_current_drive' "$CADDY_FILE" && missing_current_drive=false
  grep -qE 'push-history|my_t_push_history' "$CADDY_FILE" && missing_push_history=false
  # Accept either broad notifications matcher or explicit software + live-activity paths
  if grep -qE 'my_t_notifications|path_regexp.*notifications/|notifications/\.\*' "$CADDY_FILE" \
    || { grep -qE 'software-update/(status|pair)|my_t_software_push' "$CADDY_FILE" \
      && grep -qE 'live-activity|charging-live-activity|navigation-live-activity' "$CADDY_FILE"; }; then
    missing_notifications=false
  fi

  if [[ "$missing_states" == true || "$missing_parking_events" == true || "$missing_capabilities" == true || "$missing_current_drive" == true || "$missing_push_history" == true || "$missing_notifications" == true ]]; then
    route_anchor="$(grep -nE '^[[:space:]]*handle[[:space:]]+@(teslamate_api|api)' "$CADDY_FILE" | head -n 1 | cut -d: -f1 || true)"
    if [[ -z "$route_anchor" ]]; then
      fail "Service is healthy, but the Caddy API route location could not be detected. Add the routes from Caddyfile.snippet manually."
    fi
    backup="$CADDY_FILE.before-my-t-companion.$(date +%Y%m%d-%H%M%S)"
    cp "$CADDY_FILE" "$backup"
    route_file="$(mktemp)"
    printf '\t# BEGIN MY T VPS COMPANION\n' > "$route_file"
    if [[ "$missing_states" == true ]]; then
      cat >> "$route_file" <<'CADDY'
	@my_t_parking_states path_regexp my_t_parking_states ^/api/v1/cars/[0-9]+/states$
	handle @my_t_parking_states {
		reverse_proxy 127.0.0.1:8083
	}

CADDY
    fi
    if [[ "$missing_parking_events" == true ]]; then
      cat >> "$route_file" <<'CADDY'
	@my_t_parking_events path_regexp my_t_parking_events ^/api/v1/cars/[0-9]+/parking-events$
	handle @my_t_parking_events {
		reverse_proxy 127.0.0.1:8083
	}

CADDY
    fi
    if [[ "$missing_current_drive" == true ]]; then
      cat >> "$route_file" <<'CADDY'
	@my_t_current_drive path_regexp my_t_current_drive ^/api/v1/cars/[0-9]+/navigation/current-drive$
	handle @my_t_current_drive {
		reverse_proxy 127.0.0.1:8083
	}

CADDY
    fi
    if [[ "$missing_push_history" == true ]]; then
      cat >> "$route_file" <<'CADDY'
	@my_t_push_history path_regexp my_t_push_history ^/api/v1/cars/[0-9]+/navigation/push-history$
	handle @my_t_push_history {
		reverse_proxy 127.0.0.1:8083
	}

CADDY
    fi
    if [[ "$missing_capabilities" == true ]]; then
      cat >> "$route_file" <<'CADDY'
	@my_t_parking_capabilities path /api/v1/capabilities
	handle @my_t_parking_capabilities {
		reverse_proxy 127.0.0.1:8083
	}

CADDY
    fi
    if [[ "$missing_notifications" == true ]]; then
      cat >> "$route_file" <<'CADDY'
	@my_t_notifications path_regexp my_t_notifications ^/api/v1/notifications/
	handle @my_t_notifications {
		reverse_proxy 127.0.0.1:8083
	}

CADDY
    fi
    printf '\t# END MY T VPS COMPANION\n\n' >> "$route_file"
    awk -v line="$route_anchor" -v insert="$route_file" '
      NR == line {
        while ((getline value < insert) > 0) print value
        close(insert)
      }
      { print }
    ' "$CADDY_FILE" > "$CADDY_FILE.new"
    mv "$CADDY_FILE.new" "$CADDY_FILE"
    chmod 0644 "$CADDY_FILE"
    rm -f "$route_file"

    if ! caddy validate --config "$CADDY_FILE"; then
      cp "$backup" "$CADDY_FILE"
      chmod 0644 "$CADDY_FILE"
      fail "Caddy validation failed; the original configuration was restored."
    fi
    if ! systemctl reload caddy; then
      cp "$backup" "$CADDY_FILE"
      chmod 0644 "$CADDY_FILE"
      systemctl reload caddy || true
      fail "Caddy reload failed; the original configuration was restored."
    fi
    log "Missing VPS Companion Caddy routes installed; backup: $backup"
  else
    log "All VPS Companion Caddy routes are already installed"
  fi
  proxy_ready=true
else
  log "No supported system Caddy installation detected"
fi

capabilities="$(
  curl --fail --silent --show-error \
    -H "Authorization: Bearer $api_token" \
    http://127.0.0.1:8083/api/v1/capabilities
)"
printf '%s' "$capabilities" | grep -q '"parking_state_history"' \
  || fail "Capability verification failed."
printf '%s' "$capabilities" | grep -q '"current_drive_trajectory"' \
  || fail "Current-drive capability verification failed."
printf '%s' "$capabilities" | grep -q '"vehicle_software_update_events"' \
  || fail "Software-update capability verification failed."

# --- Wire Companion into the same URL My T already uses for TeslaMateAPI ---
# Does NOT modify TeslaMateAPI software — only front reverse-proxy path rules
# (system Caddy, docker Caddyfile in TESLAMATE_DIR, or host-edge on the API port).

setup_docker_teslamate_caddy_routes() {
  local caddyfile="$TESLAMATE_DIR/Caddyfile"
  [[ -f "$caddyfile" ]] || return 1
  grep -qE 'teslamateapi|/api/\*' "$caddyfile" 2>/dev/null || return 1
  if grep -qE 'api/v1/capabilities|my_t_parking_capabilities|my_t_capabilities' "$caddyfile" 2>/dev/null; then
    log "TeslaMate docker Caddyfile already has Companion routes"
    return 0
  fi
  local backup="$caddyfile.before-my-t-companion.$(date +%Y%m%d-%H%M%S)"
  cp "$caddyfile" "$backup"
  # Insert companion path rules before first reverse_proxy /api (HostBox / generic docker Caddy).
  local insert
  insert="$(mktemp)"
  cat > "$insert" <<'CADDY'
  # BEGIN MY T VPS COMPANION (docker edge → host loopback companion)
  @my_t_capabilities path /api/v1/capabilities
  handle @my_t_capabilities {
    reverse_proxy host.docker.internal:8083
  }
  @my_t_parking path_regexp my_t_park ^/api/v1/cars/[0-9]+/(states|parking-events)$
  handle @my_t_parking {
    reverse_proxy host.docker.internal:8083
  }
  @my_t_nav path_regexp my_t_nav ^/api/v1/cars/[0-9]+/navigation/(current-drive|push-history)$
  handle @my_t_nav {
    reverse_proxy host.docker.internal:8083
  }
  @my_t_push path_regexp my_t_push ^/api/v1/notifications/
  handle @my_t_push {
    reverse_proxy host.docker.internal:8083
  }
  # END MY T VPS COMPANION

CADDY
  # Docker Caddy cannot reach a host loopback listener. Bind Companion only to
  # Docker's private bridge gateway so host.docker.internal can reach it without
  # publishing port 8083 on any LAN/public interface.
  if grep -q '127.0.0.1:8083:8080' "$COMPOSE_FILE" 2>/dev/null; then
    companion_bridge_gateway="$(
      docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true
    )"
    [[ "$companion_bridge_gateway" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      || fail "Could not resolve Docker's private bridge gateway; refusing to expose Companion port 8083."
    sed -i.bak \
      "s/127\\.0\\.0\\.1:8083:8080/${companion_bridge_gateway}:8083:8080/g" \
      "$COMPOSE_FILE"
    rm -f "$COMPOSE_FILE.bak"
    docker compose --project-name "$COMPOSE_PROJECT" --env-file "$ENV_FILE" --file "$COMPOSE_FILE" up -d
  fi
  awk -v insert="$insert" '
    BEGIN { inserted=0 }
    /reverse_proxy \/api/ && !inserted {
      while ((getline line < insert) > 0) print line
      close(insert)
      inserted=1
    }
    { print }
  ' "$caddyfile" > "$caddyfile.new"
  mv "$caddyfile.new" "$caddyfile"
  rm -f "$insert"
  (cd "$TESLAMATE_DIR" && docker compose up -d caddy 2>/dev/null; docker compose restart caddy 2>/dev/null) || true
  log "Patched $caddyfile (backup $backup); Companion is reachable only on Docker's private bridge gateway"
  return 0
}

# Put a thin edge on the public API host port so My T keeps the same base_url.
# TeslaMateAPI stays stock; only host port mapping moves to loopback + edge.
setup_api_port_edge() {
  local compose="$TESLAMATE_DIR/docker-compose.yml"
  [[ -f "$compose" ]] || return 1

  local host_port=""
  # Prefer teslamateapi service port mapping (HostBox: "8081:8080", docs: "8080:8080")
  host_port="$(
    sed -n '/teslamateapi:/,/^  [a-zA-Z]/p' "$compose" 2>/dev/null \
      | grep -E '[0-9.]+:[0-9]+:8080|[0-9]+:8080' | head -n 1 \
      | sed -E 's/.*"([0-9.]+):([0-9]+):8080".*/\2/; t; s/.*"([0-9]+):8080".*/\1/; t; s/.*:([0-9]+):8080.*/\1/; t; s/.*[^0-9]([0-9]+):8080.*/\1/' \
      || true
  )"
  host_port="$(printf '%s' "$host_port" | tr -cd '0-9')"
  if [[ -z "$host_port" ]]; then
    if grep -qE '8081:8080' "$compose"; then host_port=8081
    elif grep -qE '[" ]8080:8080' "$compose"; then host_port=8080
    else
      log "Could not detect TeslaMateAPI host port in $compose"
      return 1
    fi
  fi

  local internal_port=18081
  log "Configuring unified edge on public API port :$host_port (API loopback :$internal_port)"

  local backup="$compose.before-my-t-companion.$(date +%Y%m%d-%H%M%S)"
  cp "$compose" "$backup"

  # Point published API mapping at loopback internal port so edge can own :host_port.
  if grep -qE "127\.0\.0\.1:${internal_port}:8080|\"${internal_port}:8080\"" "$compose"; then
    log "API already on loopback :$internal_port"
  else
    # Replace 8081:8080 or 0.0.0.0:8081:8080 style under file (best-effort)
    sed -i.bak -E \
      "s/\"?([0-9.]+:)?${host_port}:8080\"?/\"127.0.0.1:${internal_port}:8080\"/g" \
      "$compose"
    rm -f "$compose.bak"
  fi

  install -d -m 0755 "$INSTALL_DIR/edge"
  cat > "$INSTALL_DIR/edge/Caddyfile" <<CADDY
# Unified My T entry — same port as before. Stock TeslaMateAPI is not modified.
:${host_port} {
	@my_t_companion path_regexp my_t_companion ^/api/v1/(capabilities|cars/[0-9]+/states|cars/[0-9]+/parking-events|cars/[0-9]+/navigation/current-drive|cars/[0-9]+/navigation/push-history|notifications/.*)\$
	handle @my_t_companion {
		reverse_proxy 127.0.0.1:8083
	}
	handle {
		reverse_proxy 127.0.0.1:${internal_port}
	}
}
CADDY

  cat > "$INSTALL_DIR/edge/docker-compose.yml" <<YAML
services:
  my-t-api-edge:
    image: caddy:2.8-alpine@sha256:af32e97399febea808609119bb21544d0265c58a02836576e32a2d082c262c17
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
YAML

  (cd "$TESLAMATE_DIR" && docker compose up -d 2>&1 | tail -15) || true
  docker compose --project-name my-t-api-edge --file "$INSTALL_DIR/edge/docker-compose.yml" up -d 2>&1 | tail -15 \
    || fail "Failed to start API edge on :$host_port"

  # Verify local edge (what phones hit as http://IP:host_port)
  local ok=false
  for _ in $(seq 1 15); do
    if curl --fail --silent --show-error \
      -H "Authorization: Bearer $api_token" \
      "http://127.0.0.1:${host_port}/api/v1/capabilities" 2>/dev/null \
      | grep -q 'my-t-companion\|parking_state_history'; then
      ok=true
      break
    fi
    sleep 2
  done
  if [[ "$ok" != true ]]; then
    fail "Edge on :$host_port did not serve Companion capabilities. Compose backup: $backup"
  fi

  # Persist for HostBox / reinstall
  {
    printf 'MY_T_PUBLIC_API_PORT=%s\n' "$host_port"
    printf 'MY_T_API_LOOPBACK_PORT=%s\n' "$internal_port"
  } >> "$ENV_FILE"

  log "Unified edge ready: My T base_url stays http://<host>:${host_port} (same as API before)"
  printf -v MY_T_BASE_URL 'http://127.0.0.1:%s' "$host_port"
  return 0
}

if [[ "$proxy_ready" != true ]]; then
  if setup_docker_teslamate_caddy_routes; then
    # Verify via docker host port 80 if present, else fall through to api-port edge
    if curl --fail --silent --show-error \
      -H "Authorization: Bearer $api_token" \
      http://127.0.0.1/api/v1/capabilities 2>/dev/null \
      | grep -q 'my-t-companion\|parking_state_history'; then
      proxy_ready=true
      MY_T_BASE_URL="${MY_T_BASE_URL:-http://127.0.0.1}"
      log "Docker Caddy (:80) serves Companion on the public site"
    else
      log "Docker Caddy routes written; verifying via API port edge if needed"
    fi
  fi
fi

if [[ "$proxy_ready" != true ]]; then
  if setup_api_port_edge; then
    proxy_ready=true
  fi
fi

if [[ "$proxy_ready" != true ]]; then
  if [[ -n "$MY_T_BASE_URL" ]]; then
    public_capabilities_url="${MY_T_BASE_URL%/}/api/v1/capabilities"
    curl_args=(--fail --silent --show-error)
    if [[ -n "$MY_T_AUTH_HEADER" ]]; then
      curl_args+=(-H "$MY_T_AUTH_HEADER")
    else
      curl_args+=(-H "Authorization: Bearer $api_token")
    fi
    if curl "${curl_args[@]}" "$public_capabilities_url" 2>/dev/null \
      | grep -q 'my-t-companion\|parking_state_history'; then
      proxy_ready=true
      log "Unified My T endpoint verified: $public_capabilities_url"
    else
      fail "MY_T_BASE_URL set but capabilities not reachable: $public_capabilities_url"
    fi
  fi
fi

if [[ "$proxy_ready" != true ]]; then
  printf '\n'
  log "Companion process is healthy on 127.0.0.1:8083, but phone My T needs the same URL as the API."
  log "Automatic edge setup failed. Either:"
  log "  1) Install/fix Caddy or Tunnel and add Caddyfile.snippet / nginx.snippet.conf"
  log "  2) Rerun: sudo MY_T_BASE_URL=\"http://YOUR_IP:8081\" $INSTALL_DIR/install.sh"
  fail "My T cannot use Companion until the API URL serves /api/v1/capabilities."
fi

log "Installation complete (version $VERSION) — Companion is reachable on the My T API URL"
if [[ "$generated_token" == true ]]; then
  printf '\nUse this bearer token in the My T TeslaMate API connection:\n%s\n\n' "$api_token"
else
  log "The existing TeslaMate API authentication remains unchanged (same token for Companion)"
fi
log "In My T: keep the same base_url and Token; pull to refresh — 扩展 should show as available."
