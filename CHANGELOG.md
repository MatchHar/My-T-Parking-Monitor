# Changelog

## Unreleased

- Keep Companion port 8083 off public/LAN interfaces when integrating with a
  Docker-hosted gateway; fail closed if Docker's private bridge gateway cannot
  be resolved.
- Require an explicit database password and add race, vulnerability, secret,
  CodeQL, and release-provenance checks to CI.
- Require patched Go 1.25.12 for builds after the vulnerability scan identified
  reachable standard-library flaws in the original Go 1.25.0 toolchain.
- Make the security policy version-independent and clarify the authentication
  probe boundary.

## 1.10.11

- **MQTT discovery (P1):** detect docker mosquitto service **or** host `:1883`
  (HostBox system broker) and set `host.docker.internal` + `extra_hosts` automatically.
- Shared Docker network selection also considers MQTT container when present.
- **`myt-doctor.sh`:** read-only diagnostics (healthz, capabilities, MQTT hosts, edge,
  unified entry). Installed as `/opt/my-t-companion/myt-doctor.sh`.

## 1.10.10

- **Fix reported Companion version:** binary no longer hardcodes `1.10.8`. Version is
  `//go:embed` from the `VERSION` file (Dockerfile copies `VERSION` into the build).
  1.10.9 packages incorrectly still answered `capabilities.version=1.10.8`.
- Capabilities may include `teslamate_version` from env `TESLAMATE_VERSION` (installer
  fills this from the running TeslaMate container image tag) so My T can show TeslaMate
  version without scraping LiveView HTML on HostBox IP/Tunnel/Access setups.

## 1.10.9

- **Installer (P0):** no longer requires TeslaMate `.env`. Secrets are resolved from
  shell env → `docker compose config` → running containers → optional `.env` → prior
  companion install. Clear error if `DATABASE_PASS` is still missing.
- Discover `DATABASE_USER` / `DATABASE_NAME` / `DATABASE_HOST` and `MQTT_BROKER_URL`
  (or `MQTT_HOST`+`MQTT_PORT`) when present; write them into companion compose.
- Prefer a Docker network shared with TeslaMate/API when choosing `TESLAMATE_NETWORK`.
- **Gateway (P0):** `Caddyfile.snippet` / `nginx.snippet.conf` / system-Caddy install
  route all `/api/v1/notifications/*` (software-update **and** charging/navigation
  Live Activity status). LAN example routes aligned.

## 1.10.8

- Harden navigation `start_name` so destination-trip history reliably shows **from → to**:
  sticky last geofence after leaving a fence, open-drive start address, first
  reverse-geocoded position on the open drive, previous completed-drive end place,
  and mid-session backfill when TeslaMate addresses land late.
- Push payload and history continue to carry `start_name` on every navigation event.

## 1.10.7

- Record `start_name` on navigation push history (live geofence or open-drive start label) for App start → destination trip titles.
- Align English, Simplified Chinese, and Traditional Chinese documentation with
  My T 3.32 and the public My-T-App availability/setup repository.
- Correct current install/update examples, security support version, manual
  Compose image tag, and CI image naming to Companion 1.10.7.

## 1.10.6

- Mid-drive destination change ends the previous navigation session as `redirected` and starts a new session for the new destination (closed loops for destination-trip UI).

## 1.10.4

- Persist navigation push sessions and expose `GET /api/v1/cars/{id}/navigation/push-history` (`navigation_push_history`).
- Attach real trip timing on navigation end events (`trip_started_at`, `trip_ended_at`, `duration_minutes`) for authentic Live Activity end frames.
- Includes 1.10.3 history API work and 1.10.2 domain/unpair hardening.

## 1.10.2

- Added localized App compatibility metadata to `/api/v1/capabilities` so My T can safely recommend or require an App update only when the corresponding App Store version is available.
- Made `https://push.my-tesla.app/v1/events` the only trusted push relay endpoint and removed both former relay addresses from the allowlist.

## 1.10.1

- Start destination-navigation Live Activities as soon as a genuine driving
  state and destination are present; distance and ETA may arrive afterward.
- Preserve active navigation sessions across service restarts and end orphaned
  Lock Screen activities when fresh TeslaMate state does not confirm them.
- Deliver navigation end events on an independent worker so ordinary update
  retries cannot delay trip closure.

## 1.10.0

- Keep genuine parking observations long-term by default, bounded to the newest
  50,000 events instead of deleting them after 365 days.
- Bound temporary navigation, charging, and notification delivery state by age
  and entry count.
- Add durable-state backup, verified restore, and storage-status commands for
  clean VPS migration without changing TeslaMate PostgreSQL.

## 1.9.3

- Make repeated App pairing idempotent so existing software, charging, and
  navigation MQTT clients are not recreated with duplicate client IDs.
- Disconnect an existing MQTT client before applying genuinely changed pairing
  credentials, and keep a single charging/navigation delivery worker.
- Add a repeated-pairing regression test covering all three push monitors.
- Bound the Companion container to 1 CPU, 256 MB memory, and 128 processes.
- Rotate Docker JSON logs at 10 MB with three retained files.

## 1.9.2

- Fix the Caddy upgrade path so the new parking-event route is written only
  after the temporary route file has been created.
- Add a regression test for route-file initialization order.

## 1.9.1

- Fix the one-command installer so it copies the new parking-event monitor
  source into the installation directory before building the container.
- Keep 1.9.0 data/API behavior unchanged.

## 1.9.0

- Persist future-only genuine TeslaMate MQTT parking transitions for plug and
  charging, lock/Sentry/openings, climate, preconditioning, battery heating,
  and charge-port state.
- Treat each first retained MQTT value as a baseline so install and restart do
  not fabricate history.
- Add an authenticated date-range parking-event endpoint with 365-day default
  retention and first-observed timestamp semantics.
- Advertise feature-level charging, security, and climate event capabilities
  so older and non-Companion My T installations remain unaffected.

## 1.8.0

- Renamed the product and all pre-release technical identifiers to My T
  Companion.
- Moved the canonical repository and release download paths to
  `MatchHar/My-T-Companion`.
- Added complete trilingual descriptions of parking, navigation, Live Activity,
  and software-notification capabilities.
- Kept TeslaMate PostgreSQL access read-only and preserved all security
  controls from 1.7.1.

## 1.7.1 — 2026-07-28

- Retry push-to-start with the newest complete snapshot after a Live Activity
  token becomes available instead of leaving an active session permanently
  undelivered.
- Require both current battery percentage and current rated range before
  starting a charging Live Activity.
- Immediately catch up with the latest complete charging/navigation snapshot
  after push-to-start succeeds.
- Add trailing coalesced navigation updates so the final change inside the
  15-second window is not lost.
- Shorten blocking start retries so later genuine MQTT readings can recover a
  session after token registration.

## 1.7.0 — 2026-07-28

- Added privacy-minimal TeslaMate MQTT monitoring for genuine active
  destination navigation.
- Added remote navigation Live Activity push-to-start, update, and end events
  for compatible My T builds.
- Added remaining distance/time, arrival battery, and verified current-drive
  progress without transmitting coordinates or trajectory through the relay.
- Preserved the last valid destination while ending an activity and supported
  destination changes during an active drive.
- Added authenticated navigation delivery status while keeping navigation
  optional and preserving the ordinary in-App destination card without VPS.

## 1.6.1 — 2026-07-28

- Added privacy-minimal TeslaMate MQTT monitoring for genuine charging start,
  update, and end events.
- Added ActivityKit push-to-start support so My T can present a charging Live
  Activity while the App is not open.
- Added true battery percentage, rated-range gain, charging power, remaining
  time, and completion-time updates at 45 seconds normally and 15 seconds at
  50 kW or above.
- Added an authenticated charging Live Activity delivery-status endpoint.
- Continued to exclude VIN, location, routes, TeslaMate credentials, and kWh
  from the charging push payload.

## 1.5.1

- Upgrade Eclipse Paho MQTT to 1.5.1.
- Upgrade `golang.org/x/net` to 0.55.0 to address published networking,
  parser, and proxy security advisories.
- Build with Go 1.25. No API, pairing, database, or deployment behavior
  changes.

## 1.5.0

- Subscribe to TeslaMate's genuine MQTT software-update fields.
- Persist per-car state and delivered event IDs across container restarts.
- Send privacy-minimal, HMAC-SHA256 signed events to a configured HTTPS My T
  APNs relay with bounded retry.
- Add authenticated notification status at
  `/api/v1/notifications/software-update/status`.
- Keep push disabled unless installation ID, relay URL, and relay secret are
  configured together. Payloads exclude VIN, location, TeslaMate credentials,
  battery, route, and driving history.

## 1.4.1

### Added

- Immutable, checksummed GitHub Release installation and update flow.
- Installed updater that can select a version and back up the current
  installation before applying it.
- Unified-route verification for the capabilities, parking-state, and
  current-drive endpoints.
- LAN Caddy and host Nginx examples, plus explicit guidance for Traefik,
  containerized Caddy, VPN, and direct-LAN installations.
- CI, deterministic release-archive generation, contribution guidance, safe
  issue templates, and a public-release checklist.

### Changed

- Full install success is reported only when both the loopback service and the
  authenticated My T base URL work.
- Documentation now distinguishes files/routes created by the installer from
  TeslaMate data, which is never copied or modified.

### Security

- Release archives must be verified against their published SHA-256 manifests.
- Update failures retain recovery backups and do not silently replace the
  working installation.

### Compatibility and known limits

- Works with VPS and private-LAN TeslaMate deployments when TeslaMateAPI and
  the companion share one protected reverse-proxy address.
- Nginx, Traefik, containerized Caddy, and custom proxies may require manual
  route configuration.
- The companion cannot recover samples TeslaMate never stored, and it does not
  add Parking Monitor screens to My T versions that do not support them.

## 1.4.0

- Install all three reverse-proxy routes on a clean Caddy deployment.
- Enforce PostgreSQL read-only transactions.
- Run the container as a non-root user with a read-only filesystem, no Linux
  capabilities, and `no-new-privileges`.
- Add database-aware container health checking and HTTP server timeouts.
- Compare direct API tokens in constant time.
- Refuse authentication reuse when the existing `/api/ping` probe is publicly
  accessible.
- Add repeatable uninstall support, baseline unit tests, compatibility matrix,
  and security guidance.
- Unify service, image, Compose, and API release-candidate version metadata.
- Allow `update.sh` to run safely from the installed directory without trying
  to copy source files onto themselves.

## 1.3.0

- Add reliable current-drive trajectory and incremental point paging.
- Preserve immutable first-point semantics for live navigation.

## 1.2.0

- Reject stale parking boundary telemetry and expose observation timestamps.

## 1.1.0

- Reuse the existing TeslaMate API authentication boundary.

## 1.0.0

- Initial private parking state-history implementation.

## 1.10.9
- install.sh: after process start, auto-wire Companion onto the same My T API URL (system Caddy, docker Caddyfile, or host edge on the API port). Verifies /api/v1/capabilities on that URL. Does not modify TeslaMateAPI.
