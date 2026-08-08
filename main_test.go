package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIVersionMatchesReleaseVersion(t *testing.T) {
	releaseVersion, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	if got, want := apiVersion, strings.TrimSpace(string(releaseVersion)); got != want {
		t.Fatalf("apiVersion %q does not match VERSION %q", got, want)
	}
}

func TestInstallerIncludesParkingEventMonitor(t *testing.T) {
	installer, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	text := string(installer)
	for _, required := range []string{
		`-f "$SOURCE_DIR/parking_event_monitor.go"`,
		`install -m 0644 "$SOURCE_DIR/parking_event_monitor.go" "$INSTALL_DIR/parking_event_monitor.go"`,
		`install -m 0644 "$SOURCE_DIR/storage_policy.go" "$INSTALL_DIR/storage_policy.go"`,
		`install -m 0755 "$SOURCE_DIR/backup.sh" "$INSTALL_DIR/backup.sh"`,
		`install -m 0755 "$SOURCE_DIR/restore.sh" "$INSTALL_DIR/restore.sh"`,
		`install -m 0755 "$SOURCE_DIR/storage-status.sh" "$INSTALL_DIR/storage-status.sh"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("installer is missing required parking monitor handling: %s", required)
		}
	}
	routeFileInitialization := strings.Index(text, `route_file="$(mktemp)"`)
	parkingRouteWrite := strings.Index(text, `if [[ "$missing_parking_events" == true ]]; then`)
	if routeFileInitialization < 0 || parkingRouteWrite < 0 || routeFileInitialization > parkingRouteWrite {
		t.Fatal("parking event route must be written only after route_file is initialized")
	}
}

func TestInstallerNeverPublishesCompanionOnAllInterfaces(t *testing.T) {
	installer, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{
		"s/127\\.0\\.0\\.1:8083:8080/8083:8080",
		`- "8083:8080"`,
	} {
		if strings.Contains(string(installer), unsafe) {
			t.Fatalf("installer contains unsafe public Companion mapping %q", unsafe)
		}
	}
}

func TestTokenEqual(t *testing.T) {
	t.Parallel()
	if !tokenEqual("correct-token", "correct-token") {
		t.Fatal("equal tokens must match")
	}
	for _, candidate := range []string{"", "wrong-token", "correct-token-extra"} {
		if tokenEqual(candidate, "correct-token") {
			t.Fatalf("unexpected token match for %q", candidate)
		}
	}
}

func TestSoftwareNotificationRelaySignatureAndPrivacy(t *testing.T) {
	t.Parallel()
	const secret = "relay-secret"
	var received softwareNotificationEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		signature := hmac.New(sha256.New, []byte(secret))
		_, _ = signature.Write(body)
		want := "sha256=" + hex.EncodeToString(signature.Sum(nil))
		if !hmac.Equal([]byte(r.Header.Get("X-My-T-Signature")), []byte(want)) {
			t.Fatal("relay signature mismatch")
		}
		if strings.Contains(string(body), "VIN") || strings.Contains(string(body), "latitude") {
			t.Fatal("payload contains prohibited vehicle data")
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	monitor := &softwareNotificationMonitor{
		relayURL:       server.URL,
		relaySecret:    secret,
		installationID: "installation-1",
		httpClient:     server.Client(),
	}
	event := softwareNotificationEvent{
		EventID:        "event-1",
		InstallationID: "installation-1",
		CarID:          1,
		VehicleName:    "MY CAR",
		Type:           "update_available",
		CurrentVersion: "2026.20.6",
		UpdateVersion:  "2026.26.3",
		ObservedAt:     "2026-07-27T12:00:00Z",
	}
	if err := monitor.deliver(event); err != nil {
		t.Fatal(err)
	}
	if received.UpdateVersion != event.UpdateVersion || received.InstallationID != event.InstallationID {
		t.Fatalf("unexpected relay event: %+v", received)
	}
}

func TestSoftwareNotificationStatePersistence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	monitor := &softwareNotificationMonitor{
		statePath: path,
		store: softwareNotificationStore{
			Cars: map[int]carSoftwareState{
				1: {Version: "2026.20.6", UpdateAvailable: true, UpdateVersion: "2026.26.3"},
			},
			Delivered: map[string]string{"event-1": "2026-07-27T12:00:00Z"},
		},
	}
	monitor.mu.Lock()
	if err := monitor.saveLocked(); err != nil {
		t.Fatal(err)
	}
	monitor.mu.Unlock()

	loaded := &softwareNotificationMonitor{
		statePath: path,
		store: softwareNotificationStore{
			Cars:      map[int]carSoftwareState{},
			Delivered: map[string]string{},
		},
	}
	loaded.load()
	if loaded.store.Cars[1].UpdateVersion != "2026.26.3" {
		t.Fatalf("state was not restored: %+v", loaded.store)
	}
	if _, ok := loaded.store.Delivered["event-1"]; !ok {
		t.Fatal("delivered event deduplication was not restored")
	}
}

func TestSoftwarePushPairingRejectsUntrustedRelay(t *testing.T) {
	for _, relayURL := range []string{
		"https://example.invalid/events",
		"https://my-t-push.samman.top/v1/events",
		"https://teslamate-api.samman.top/my-t-push/v1/events",
	} {
		monitor := &softwareNotificationMonitor{
			statePath: filepath.Join(t.TempDir(), "software-notifications.json"),
		}
		err := monitor.configure(softwarePushPairing{
			InstallationID: strings.Repeat("a", 48),
			RelayURL:       relayURL,
			RelaySecret:    strings.Repeat("b", 64),
		})
		if err == nil {
			t.Fatalf("expected untrusted relay URL %q to be rejected", relayURL)
		}
	}
}

func TestRepeatedPairingDoesNotRestartMQTTMonitorsOrWorkers(t *testing.T) {
	pairing := softwarePushPairing{
		InstallationID: strings.Repeat("a", 48),
		RelayURL:       officialSoftwarePushRelayURL,
		RelaySecret:    strings.Repeat("b", 64),
	}
	statePath := filepath.Join(t.TempDir(), "software.json")
	software := &softwareNotificationMonitor{
		statePath:      statePath,
		installationID: pairing.InstallationID,
		relayURL:       pairing.RelayURL,
		relaySecret:    pairing.RelaySecret,
		enabled:        true,
		started:        true,
	}
	charging := &chargingNotificationMonitor{
		installationID: pairing.InstallationID,
		relayURL:       pairing.RelayURL,
		relaySecret:    pairing.RelaySecret,
		enabled:        true,
		started:        true,
		workerStarted:  true,
	}
	navigation := &navigationNotificationMonitor{
		installationID: pairing.InstallationID,
		relayURL:       pairing.RelayURL,
		relaySecret:    pairing.RelaySecret,
		enabled:        true,
		started:        true,
		workerStarted:  true,
	}

	for attempt := 0; attempt < 3; attempt++ {
		if err := software.configure(pairing); err != nil {
			t.Fatalf("software repeated pairing %d: %v", attempt, err)
		}
		if err := charging.configure(pairing); err != nil {
			t.Fatalf("charging repeated pairing %d: %v", attempt, err)
		}
		if err := navigation.configure(pairing); err != nil {
			t.Fatalf("navigation repeated pairing %d: %v", attempt, err)
		}
	}

	if !software.started || !charging.started || !navigation.started {
		t.Fatal("repeated pairing must not reset a running MQTT monitor")
	}
	if !charging.workerStarted || !navigation.workerStarted {
		t.Fatal("repeated pairing must preserve the single delivery worker")
	}
	if _, err := os.Stat(software.pairingPath()); err != nil {
		t.Fatalf("repeated pairing must still persist pairing state: %v", err)
	}
}

func TestDisablePairingClearsPersistentAndRuntimeState(t *testing.T) {
	pairing := softwarePushPairing{
		InstallationID: strings.Repeat("a", 48),
		RelayURL:       officialSoftwarePushRelayURL,
		RelaySecret:    strings.Repeat("b", 64),
	}
	software := &softwareNotificationMonitor{
		statePath:      filepath.Join(t.TempDir(), "software.json"),
		installationID: pairing.InstallationID,
		relayURL:       pairing.RelayURL,
		relaySecret:    pairing.RelaySecret,
		enabled:        true,
	}
	data, _ := json.Marshal(pairing)
	if err := os.WriteFile(software.pairingPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := software.disable(); err != nil {
		t.Fatal(err)
	}
	if software.enabled || software.installationID != "" || software.relayURL != "" || software.relaySecret != "" {
		t.Fatal("software pairing remained enabled after disable")
	}
	if _, err := os.Stat(software.pairingPath()); !os.IsNotExist(err) {
		t.Fatalf("pairing file still exists after disable: %v", err)
	}
}

func TestNavigationCanStartBeforeOptionalMetricsArrive(t *testing.T) {
	state := carNavigationState{
		VehicleState: "driving",
		Destination:  "Central Park",
	}
	if !navigationShouldBeActive(false, state) {
		t.Fatal("destination navigation should not wait for optional distance/minutes")
	}
	if navigationShouldBeActive(true, state) {
		t.Fatal("invalid active_route must not remain active")
	}
}

func TestNavigationEndUsesPriorityQueue(t *testing.T) {
	monitor := &navigationNotificationMonitor{
		queue:         make(chan navigationLiveActivityEvent, 1),
		priorityQueue: make(chan navigationLiveActivityEvent, 1),
	}
	event := navigationLiveActivityEvent{
		EventID: "end-1",
		Type:    "navigation_ended",
	}
	monitor.enqueue(event)
	select {
	case received := <-monitor.priorityQueue:
		if received.EventID != event.EventID {
			t.Fatalf("unexpected priority event: %+v", received)
		}
	default:
		t.Fatal("navigation end was not queued with priority")
	}
}

func TestChargingEventUsesOnlyRequiredTelemetryAndValidSignature(t *testing.T) {
	t.Parallel()
	const secret = "relay-secret"
	var received chargingLiveActivityEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		signature := hmac.New(sha256.New, []byte(secret))
		_, _ = signature.Write(body)
		want := "sha256=" + hex.EncodeToString(signature.Sum(nil))
		if !hmac.Equal([]byte(r.Header.Get("X-My-T-Signature")), []byte(want)) {
			t.Fatal("relay signature mismatch")
		}
		for _, prohibited := range []string{"vin", "latitude", "longitude", "address", "teslamate_token"} {
			if strings.Contains(strings.ToLower(string(body)), prohibited) {
				t.Fatalf("payload contains prohibited field %q", prohibited)
			}
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	power, startRange, currentRange := 10.8, 171.0, 254.0
	addedRange := currentRange - startRange
	remaining := 4200
	eta := time.Date(2026, 7, 28, 23, 10, 0, 0, time.UTC).Unix()
	monitor := &chargingNotificationMonitor{
		relayURL:       server.URL,
		relaySecret:    secret,
		installationID: "installation-1",
		httpClient:     server.Client(),
	}
	event := chargingLiveActivityEvent{
		EventID:             "event-1",
		InstallationID:      "installation-1",
		CarID:               1,
		VehicleName:         "MY CAR",
		Type:                "charging_updated",
		SessionID:           "charge-1234567890abcdef",
		StartBatteryLevel:   35,
		BatteryLevel:        52,
		AddedBatteryPercent: 17,
		TargetLevel:         80,
		StartRatedRangeKM:   &startRange,
		RatedRangeKM:        &currentRange,
		AddedRangeKM:        &addedRange,
		PowerKW:             &power,
		RemainingSeconds:    &remaining,
		EstimatedCompleteAt: &eta,
		ObservedAt:          "2026-07-28T22:00:00Z",
	}
	if err := monitor.deliver(event); err != nil {
		t.Fatal(err)
	}
	if received.SessionID != event.SessionID || received.BatteryLevel != 52 ||
		received.AddedBatteryPercent != 17 ||
		received.AddedRangeKM == nil || *received.AddedRangeKM != addedRange {
		t.Fatalf("unexpected relay event: %+v", received)
	}
}

func TestChargingSessionLifecycleUsesTrueBoundaryBattery(t *testing.T) {
	t.Parallel()
	monitor := &chargingNotificationMonitor{
		installationID: strings.Repeat("a", 48),
		pending:        map[int]*time.Timer{},
		queue:          make(chan chargingLiveActivityEvent, 8),
		store: chargingNotificationStore{
			Cars:      map[int]carChargingState{},
			Delivered: map[string]string{},
		},
		statePath: filepath.Join(t.TempDir(), "charging.json"),
	}
	at := time.Date(2026, 7, 28, 22, 0, 0, 0, time.UTC)
	monitor.observe(1, "state", "charging", at)
	if len(monitor.queue) != 0 {
		t.Fatal("must wait for a genuine battery observation before starting")
	}
	monitor.observe(1, "charge_limit_soc", "80", at)
	monitor.observe(1, "rated_battery_range_km", "171", at)
	monitor.observe(1, "battery_level", "35", at.Add(time.Second))
	start := <-monitor.queue
	if start.Type != "charging_started" || start.StartBatteryLevel != 35 ||
		start.BatteryLevel != 35 || start.TargetLevel != 80 {
		t.Fatalf("unexpected start event: %+v", start)
	}
	monitor.mu.Lock()
	state := monitor.store.Cars[1]
	state.StartDelivered = true
	monitor.store.Cars[1] = state
	monitor.mu.Unlock()
	monitor.observe(1, "battery_level", "36", at.Add(time.Minute))
	update := <-monitor.queue
	if update.Type != "charging_updated" || update.StartBatteryLevel != 35 ||
		update.BatteryLevel != 36 || update.AddedBatteryPercent != 1 {
		t.Fatalf("unexpected update event: %+v", update)
	}
	monitor.observe(1, "state", "online", at.Add(2*time.Minute))
	end := <-monitor.queue
	if end.Type != "charging_ended" || end.StartBatteryLevel != 35 ||
		end.BatteryLevel != 36 {
		t.Fatalf("unexpected end event: %+v", end)
	}
}

func TestParkingEventsIgnoreRetainedBaselineAndPersistRealTransitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parking-events.json")
	monitor := &parkingEventMonitor{
		statePath:     path,
		retentionDays: 365,
		store: parkingEventStore{
			Cars:   map[int]parkingEventCarState{},
			Events: []parkingObservedEvent{},
		},
	}
	at := time.Date(2026, 7, 28, 22, 0, 0, 0, time.UTC)
	monitor.observe(1, "battery_level", "35", at)
	monitor.observe(1, "rated_battery_range_km", "171", at)
	monitor.observe(1, "plugged_in", "false", at)
	monitor.observe(1, "charging_state", "Disconnected", at)
	if len(monitor.store.Events) != 0 {
		t.Fatalf("retained baseline must not create events: %+v", monitor.store.Events)
	}

	monitor.observe(1, "plugged_in", "true", at.Add(time.Minute))
	monitor.observe(1, "charging_state", "NoPower", at.Add(2*time.Minute))
	monitor.observe(1, "charging_state", "Charging", at.Add(3*time.Minute))
	monitor.observe(1, "battery_level", "36", at.Add(4*time.Minute))
	monitor.observe(1, "charging_state", "Complete", at.Add(5*time.Minute))
	monitor.observe(1, "plugged_in", "false", at.Add(6*time.Minute))

	wantTypes := []string{
		"plug_connected",
		"charging_no_power",
		"charging_started",
		"charging_completed",
		"plug_disconnected",
	}
	if len(monitor.store.Events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(monitor.store.Events), len(wantTypes), monitor.store.Events)
	}
	for index, want := range wantTypes {
		if got := monitor.store.Events[index].Type; got != want {
			t.Fatalf("event %d type=%q, want %q", index, got, want)
		}
	}
	if event := monitor.store.Events[2]; event.BatteryLevel == nil || *event.BatteryLevel != 35 ||
		event.RatedRangeKM == nil || *event.RatedRangeKM != 171 ||
		event.ObservationMode != "teslamate_mqtt_first_observed" {
		t.Fatalf("unexpected boundary telemetry: %+v", event)
	}

	loaded := &parkingEventMonitor{
		statePath: path,
		store: parkingEventStore{
			Cars:   map[int]parkingEventCarState{},
			Events: []parkingObservedEvent{},
		},
	}
	loaded.load()
	if len(loaded.store.Events) != len(wantTypes) {
		t.Fatalf("persisted events not restored: %+v", loaded.store.Events)
	}
}

func TestParkingSecurityAndClimateTransitions(t *testing.T) {
	monitor := &parkingEventMonitor{
		statePath:     filepath.Join(t.TempDir(), "parking-events.json"),
		retentionDays: 365,
		store: parkingEventStore{
			Cars:   map[int]parkingEventCarState{},
			Events: []parkingObservedEvent{},
		},
	}
	at := time.Date(2026, 7, 28, 22, 0, 0, 0, time.UTC)
	fields := []string{
		"locked", "sentry_mode", "doors_open", "windows_open",
		"trunk_open", "frunk_open", "is_climate_on",
		"is_preconditioning", "battery_heater",
	}
	for _, field := range fields {
		monitor.observe(1, field, "false", at)
		monitor.observe(1, field, "true", at.Add(time.Minute))
	}
	want := []string{
		"vehicle_locked",
		"sentry_enabled",
		"doors_opened",
		"windows_opened",
		"trunk_opened",
		"frunk_opened",
		"climate_started",
		"preconditioning_started",
		"battery_heating_started",
	}
	if len(monitor.store.Events) != len(want) {
		t.Fatalf("got events %+v", monitor.store.Events)
	}
	for index, eventType := range want {
		if monitor.store.Events[index].Type != eventType {
			t.Fatalf("event %d=%q, want %q", index, monitor.store.Events[index].Type, eventType)
		}
	}
}

func TestParsePageLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  int
		ok    bool
	}{
		{"", 5000, true},
		{"1", 1, true},
		{"10000", 10000, true},
		{"0", 0, false},
		{"10001", 0, false},
		{"invalid", 0, false},
	}
	for _, test := range tests {
		got, err := parsePageLimit(test.value)
		if (err == nil) != test.ok || got != test.want {
			t.Errorf("parsePageLimit(%q) = %d, %v; want %d, ok=%v", test.value, got, err, test.want, test.ok)
		}
	}
}

func TestParseDateParam(t *testing.T) {
	t.Parallel()
	got, err := parseDateParam("2026-07-27T12:30:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 27, 4, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
	if _, err := parseDateParam(""); err == nil {
		t.Fatal("missing date must fail")
	}
}

func TestCapabilitiesRequiresAuthentication(t *testing.T) {
	oldToken, oldProbe := apiToken, authProbeURL
	apiToken, authProbeURL = "test-token", ""
	t.Cleanup(func() {
		apiToken, authProbeURL = oldToken, oldProbe
	})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	recorder := httptest.NewRecorder()
	handleCapabilities(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder = httptest.NewRecorder()
	handleCapabilities(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", recorder.Code)
	}
	var payload struct {
		AppCompatibility struct {
			MinimumVersion     string `json:"minimum_version"`
			RecommendedVersion string `json:"recommended_version"`
		} `json:"app_compatibility"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if payload.AppCompatibility.MinimumVersion != "3.10" || payload.AppCompatibility.RecommendedVersion != "3.30" {
		t.Fatalf("unexpected app compatibility: %+v", payload.AppCompatibility)
	}
}

func TestParkingEventsDefaultToLongTermRetentionAndCapacityLimit(t *testing.T) {
	t.Setenv("PARKING_EVENT_RETENTION_DAYS", "")
	t.Setenv("PARKING_EVENT_MAX_EVENTS", "1000")
	monitor := newParkingEventMonitorFromEnvironment()
	now := time.Now().UTC()
	for index := 0; index < 1005; index++ {
		monitor.store.Events = append(monitor.store.Events, parkingObservedEvent{
			ID:         strings.Repeat("x", index+1),
			ObservedAt: now.AddDate(-10, 0, 0).Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano),
		})
	}
	monitor.pruneLocked(now)
	if got := len(monitor.store.Events); got != 1000 {
		t.Fatalf("got %d events, want 1000", got)
	}
	wantOldest := now.AddDate(-10, 0, 0).Add(5 * time.Second).Format(time.RFC3339Nano)
	if got := monitor.store.Events[0].ObservedAt; got != wantOldest {
		t.Fatalf("oldest retained event = %q, want %q", got, wantOldest)
	}
}

func TestPruneTimestampMapByAgeAndCapacity(t *testing.T) {
	now := time.Now().UTC()
	delivered := map[string]string{
		"expired": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano),
		"oldest":  now.Add(-3 * time.Hour).Format(time.RFC3339Nano),
		"middle":  now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		"newest":  now.Add(-time.Hour).Format(time.RFC3339Nano),
		"invalid": "not-a-timestamp",
	}
	pruneTimestampMap(delivered, now, 7*24*time.Hour, 2)
	if len(delivered) != 2 || delivered["middle"] == "" || delivered["newest"] == "" {
		t.Fatalf("unexpected retained entries: %+v", delivered)
	}
}
