package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

const headerAPIVersion = "API-Version"

//go:embed VERSION
var embeddedVersion string

var (
	// Always matches the VERSION file (enforced by TestAPIVersionMatchesReleaseVersion).
	apiVersion = strings.TrimSpace(embeddedVersion)
	db         *sql.DB
	apiToken   string
	authProbeURL string
	authClient *http.Client
	location   *time.Location
	carIDPath             = regexp.MustCompile(`^/api/v1/cars/(\d+)/states$`)
	currentDrivePath      = regexp.MustCompile(`^/api/v1/cars/(\d+)/navigation/current-drive$`)
	navigationHistoryPath = regexp.MustCompile(`^/api/v1/cars/(\d+)/navigation/push-history$`)
	parkingEventsPath     = regexp.MustCompile(`^/api/v1/cars/(\d+)/parking-events$`)
	softwarePush          *softwareNotificationMonitor
	chargingPush          *chargingNotificationMonitor
	navigationPush        *navigationNotificationMonitor
	parkingEvents         *parkingEventMonitor
)

type telemetrySample struct {
	Date         string   `json:"date"`
	BatteryLevel *int     `json:"battery_level"`
	RatedRangeKM *float64 `json:"rated_battery_range_km"`
}

type carRef struct {
	CarID   int     `json:"car_id"`
	CarName *string `json:"car_name"`
}

type stateInterval struct {
	State          string           `json:"state"`
	StartDate      string           `json:"start_date"`
	EndDate        *string          `json:"end_date"`
	StartTelemetry *telemetrySample `json:"start_telemetry"`
	EndTelemetry   *telemetrySample `json:"end_telemetry"`
}

type responseData struct {
	Car    carRef          `json:"car"`
	States []stateInterval `json:"states"`
}

type envelope struct {
	Data responseData `json:"data"`
	Meta responseMeta `json:"meta"`
}

type responseMeta struct {
	GeneratedAt               string `json:"generated_at"`
	StorageMode               string `json:"storage_mode"`
	Retention                 string `json:"retention"`
	RecommendedRefreshSeconds int    `json:"recommended_refresh_seconds"`
}

type drivePoint struct {
	ID        int64    `json:"id"`
	Date      string   `json:"date"`
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Speed     *int     `json:"speed"`
	Odometer  *float64 `json:"odometer"`
}

type currentDrive struct {
	DriveID          int64        `json:"drive_id"`
	StartDate        string       `json:"start_date"`
	EndDate          *string      `json:"end_date"`
	IsOngoing        bool         `json:"is_ongoing"`
	DataStatus       string       `json:"data_status"`
	FirstPoint       *drivePoint  `json:"first_point"`
	Points           []drivePoint `json:"points"`
	ReturnedCount    int          `json:"returned_count"`
	AfterPointID     int64        `json:"after_point_id"`
	NextAfterPointID *int64       `json:"next_after_point_id"`
	HasMore          bool         `json:"has_more"`
}

type currentDriveData struct {
	Car   carRef        `json:"car"`
	Drive *currentDrive `json:"drive"`
}

type currentDriveEnvelope struct {
	Data currentDriveData `json:"data"`
	Meta responseMeta     `json:"meta"`
}

func main() {
	log.SetFlags(log.Ldate | log.Lmicroseconds)

	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		runHealthcheck()
		return
	}

	apiToken = strings.TrimSpace(os.Getenv("API_TOKEN"))
	if apiToken == "" {
		log.Fatal("[error] API_TOKEN is required")
	}
	authProbeURL = strings.TrimSpace(os.Getenv("AUTH_PROBE_URL"))
	authClient = &http.Client{Timeout: 4 * time.Second}

	tzName := getenv("TZ", "UTC")
	var err error
	location, err = time.LoadLocation(tzName)
	if err != nil {
		log.Fatalf("[error] invalid TZ %q: %v", tzName, err)
	}

	initDB()
	defer db.Close()
	softwarePush = newSoftwareNotificationMonitorFromEnvironment()
	softwarePush.start()
	defer softwarePush.stop()
	chargingPush = newChargingNotificationMonitorFromEnvironment()
	chargingPush.start()
	defer chargingPush.stop()
	navigationPush = newNavigationNotificationMonitorFromEnvironment()
	navigationPush.start()
	defer navigationPush.stop()
	parkingEvents = newParkingEventMonitorFromEnvironment()
	parkingEvents.start()
	defer parkingEvents.stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", handlePing)
	mux.HandleFunc("/api/healthz", handleHealth)
	mux.HandleFunc("/api/v1/capabilities", handleCapabilities)
	mux.HandleFunc("/api/v1/notifications/software-update/status", handleSoftwareNotificationStatus)
	mux.HandleFunc("/api/v1/notifications/software-update/pair", handleSoftwareNotificationPair)
	mux.HandleFunc("/api/v1/notifications/charging-live-activity/status", handleChargingNotificationStatus)
	mux.HandleFunc("/api/v1/notifications/navigation-live-activity/status", handleNavigationNotificationStatus)
	mux.HandleFunc("/", handleStates)

	addr := ":8080"
	log.Printf("[info] mycarmate-states-api listening on %s", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           withHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func handleSoftwareNotificationPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if r.Method == http.MethodDelete {
		if softwarePush == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Unavailable"})
			return
		}
		if err := softwarePush.disable(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unpair failed"})
			return
		}
		if chargingPush != nil {
			chargingPush.disable()
		}
		if navigationPush != nil {
			navigationPush.disable()
		}
		writeJSON(w, http.StatusOK, map[string]bool{"paired": false})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()
	var pairing softwarePushPairing
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&pairing) != nil || softwarePush == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid pairing"})
		return
	}
	if err := softwarePush.configure(pairing); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid pairing"})
		return
	}
	if chargingPush != nil {
		if err := chargingPush.configure(pairing); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid charging pairing"})
			return
		}
	}
	if navigationPush != nil {
		if err := navigationPush.configure(pairing); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid navigation pairing"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paired": true})
}

func handleChargingNotificationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if chargingPush == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, chargingPush.status())
}

func handleNavigationNotificationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if navigationPush == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, navigationPush.status())
}

func handleNavigationPushHistory(w http.ResponseWriter, r *http.Request, carIDValue string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if navigationPush == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Unavailable"})
		return
	}
	carID, err := strconv.Atoi(carIDValue)
	if err != nil || carID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid car id"})
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	sessions := navigationPush.historyForCar(carID, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

func runHealthcheck() {
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://127.0.0.1:8080/api/healthz")
	if err != nil {
		log.Printf("[error] healthcheck request: %v", err)
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		log.Printf("[error] healthcheck status: %d", response.StatusCode)
		os.Exit(1)
	}
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerAPIVersion, apiVersion)
		next.ServeHTTP(w, r)
	})
}

func handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if db == nil || db.PingContext(ctx) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "ERROR"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "OK"})
}

func handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	payload := map[string]any{
		"service": "my-t-companion",
		"version": apiVersion,
		"app_compatibility": map[string]any{
			"minimum_version":     "3.10",
			"recommended_version": "3.30",
			"release_notes": map[string]string{
				"zh_hans": "新增 App 与扩展服务双向版本检查，并改进扩展功能的安全降级。",
				"zh_hant": "新增 App 與擴充服務雙向版本檢查，並改善擴充功能的安全降級。",
				"en":      "Adds two-way App and Companion compatibility checks with safer feature fallback.",
			},
		},
		"capabilities": []string{
			"parking_state_history",
			"state_boundary_battery",
			"state_boundary_rated_range",
			"timestamped_boundary_telemetry",
			"bounded_boundary_telemetry",
			"live_open_intervals",
			"data_coverage",
			"current_drive_trajectory",
			"immutable_current_drive_start",
			"incremental_drive_points",
			"vehicle_software_update_events",
			"apns_relay_delivery_status",
			"charging_live_activity_events",
			"charging_live_activity_push_to_start",
			"charging_live_activity_remote_updates",
			"navigation_live_activity_events",
			"navigation_live_activity_push_to_start",
			"navigation_live_activity_remote_updates",
			"navigation_push_history",
			"parking_observed_events",
			"parking_charge_connection_events",
			"parking_security_events",
			"parking_climate_events",
		},
		"data_policy":                 "teslamate_read_only_observations",
		"storage_mode":                "teslamate_source_of_truth",
		"retention":                   "follows_teslamate_database",
		"recommended_refresh_seconds": 30,
		"source_tables": []string{
			"states",
			"drives",
			"positions",
			"charging_processes",
			"charges",
			"updates",
		},
	}
	// Optional: HostBox / installer can set TESLAMATE_VERSION from the running image tag
	// so My T can show TeslaMate version without scraping the LiveView HTML (:4000 / Access).
	if tm := strings.TrimSpace(os.Getenv("TESLAMATE_VERSION")); tm != "" {
		payload["teslamate_version"] = tm
	}
	writeJSON(w, http.StatusOK, payload)
}

func handleSoftwareNotificationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	if softwarePush == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, softwarePush.status())
}

func handleStates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if matches := currentDrivePath.FindStringSubmatch(r.URL.Path); len(matches) == 2 {
		handleCurrentDrive(w, r, matches[1])
		return
	}
	if matches := navigationHistoryPath.FindStringSubmatch(r.URL.Path); len(matches) == 2 {
		handleNavigationPushHistory(w, r, matches[1])
		return
	}
	if matches := parkingEventsPath.FindStringSubmatch(r.URL.Path); len(matches) == 2 {
		handleParkingEvents(w, r, matches[1])
		return
	}

	matches := carIDPath.FindStringSubmatch(r.URL.Path)
	if len(matches) != 2 {
		http.NotFound(w, r)
		return
	}

	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}

	carID, err := strconv.Atoi(matches[1])
	if err != nil || carID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid car id"})
		return
	}

	startDate, err := parseDateParam(r.URL.Query().Get("startDate"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid startDate"})
		return
	}
	endDate, err := parseDateParam(r.URL.Query().Get("endDate"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid endDate"})
		return
	}
	if !endDate.After(startDate) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endDate must be after startDate"})
		return
	}

	payload, err := fetchStates(carID, startDate, endDate)
	if err != nil {
		log.Printf("[error] fetchStates car=%d: %v", carID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to load state history"})
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Data: payload,
		Meta: responseMeta{
			GeneratedAt:               time.Now().In(location).Format(time.RFC3339),
			StorageMode:               "teslamate_source_of_truth",
			Retention:                 "follows_teslamate_database",
			RecommendedRefreshSeconds: 30,
		},
	})
}

func handleParkingEvents(w http.ResponseWriter, r *http.Request, carIDValue string) {
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	carID, err := strconv.Atoi(carIDValue)
	if err != nil || carID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid car id"})
		return
	}
	startDate, err := parseDateParam(r.URL.Query().Get("startDate"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid startDate"})
		return
	}
	endDate, err := parseDateParam(r.URL.Query().Get("endDate"))
	if err != nil || !endDate.After(startDate) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid endDate"})
		return
	}
	events := []parkingObservedEvent{}
	retentionDays := defaultParkingEventRetentionDays
	if parkingEvents != nil {
		events = parkingEvents.events(carID, startDate, endDate)
		retentionDays = parkingEvents.retentionDays
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"car":    carRef{CarID: carID},
			"events": events,
		},
		"meta": map[string]any{
			"generated_at":     time.Now().In(location).Format(time.RFC3339),
			"storage_mode":     "companion_observed_mqtt",
			"retention_days":   retentionDays,
			"timestamp_policy": "teslamate_mqtt_first_observed",
		},
	})
}

func handleCurrentDrive(w http.ResponseWriter, r *http.Request, carIDValue string) {
	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}

	carID, err := strconv.Atoi(carIDValue)
	if err != nil || carID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid car id"})
		return
	}
	afterPointID, err := parseNonNegativeInt64(r.URL.Query().Get("afterPointId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid afterPointId"})
		return
	}
	limit, err := parsePageLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid limit"})
		return
	}

	payload, err := fetchCurrentDrive(carID, afterPointID, limit)
	if err != nil {
		log.Printf("[error] fetchCurrentDrive car=%d: %v", carID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to load current drive"})
		return
	}
	writeJSON(w, http.StatusOK, currentDriveEnvelope{
		Data: payload,
		Meta: responseMeta{
			GeneratedAt:               time.Now().In(location).Format(time.RFC3339),
			StorageMode:               "teslamate_source_of_truth",
			Retention:                 "follows_teslamate_database",
			RecommendedRefreshSeconds: 3,
		},
	})
}

func fetchCurrentDrive(carID int, afterPointID int64, limit int) (currentDriveData, error) {
	data := currentDriveData{Car: carRef{CarID: carID}}
	var carName sql.NullString
	if err := db.QueryRow(`SELECT name FROM cars WHERE id = $1`, carID).Scan(&carName); err != nil {
		if err == sql.ErrNoRows {
			return data, nil
		}
		return data, err
	}
	if carName.Valid && strings.TrimSpace(carName.String) != "" {
		name := carName.String
		data.Car.CarName = &name
	}

	var driveID int64
	var startDate time.Time
	var endDate sql.NullTime
	err := db.QueryRow(`
		SELECT id, start_date, end_date
		FROM drives
		WHERE car_id = $1 AND end_date IS NULL
		ORDER BY start_date DESC, id DESC
		LIMIT 1`, carID).Scan(&driveID, &startDate, &endDate)
	if err == sql.ErrNoRows {
		return data, nil
	}
	if err != nil {
		return data, err
	}

	drive := &currentDrive{
		DriveID:      driveID,
		StartDate:    startDate.In(location).Format(time.RFC3339),
		IsOngoing:    !endDate.Valid,
		DataStatus:   "waiting_for_positions",
		Points:       []drivePoint{},
		AfterPointID: afterPointID,
	}
	if endDate.Valid {
		value := endDate.Time.In(location).Format(time.RFC3339)
		drive.EndDate = &value
	}

	first, err := fetchDrivePoint(driveID)
	if err != nil {
		return data, err
	}
	drive.FirstPoint = first

	rows, err := db.Query(`
		SELECT id, date, latitude, longitude, speed, odometer
		FROM positions
		WHERE drive_id = $1
		  AND id > $2
		  AND latitude BETWEEN -90 AND 90
		  AND longitude BETWEEN -180 AND 180
		ORDER BY id ASC
		LIMIT $3`, driveID, afterPointID, limit+1)
	if err != nil {
		return data, err
	}
	defer rows.Close()

	for rows.Next() {
		point, err := scanDrivePoint(rows)
		if err != nil {
			return data, err
		}
		if len(drive.Points) == limit {
			drive.HasMore = true
			break
		}
		drive.Points = append(drive.Points, point)
	}
	if err := rows.Err(); err != nil {
		return data, err
	}
	drive.ReturnedCount = len(drive.Points)
	if drive.ReturnedCount > 0 {
		next := drive.Points[drive.ReturnedCount-1].ID
		drive.NextAfterPointID = &next
		drive.DataStatus = "available"
	} else if drive.FirstPoint != nil {
		drive.DataStatus = "available"
	}
	data.Drive = drive
	return data, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func fetchDrivePoint(driveID int64) (*drivePoint, error) {
	row := db.QueryRow(`
		SELECT id, date, latitude, longitude, speed, odometer
		FROM positions
		WHERE drive_id = $1
		  AND latitude BETWEEN -90 AND 90
		  AND longitude BETWEEN -180 AND 180
		ORDER BY id ASC
		LIMIT 1`, driveID)
	point, err := scanDrivePoint(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &point, nil
}

func scanDrivePoint(row rowScanner) (drivePoint, error) {
	var point drivePoint
	var date time.Time
	var speed sql.NullInt64
	var odometer sql.NullFloat64
	if err := row.Scan(&point.ID, &date, &point.Latitude, &point.Longitude, &speed, &odometer); err != nil {
		return drivePoint{}, err
	}
	point.Date = date.In(location).Format(time.RFC3339Nano)
	if speed.Valid {
		value := int(speed.Int64)
		point.Speed = &value
	}
	if odometer.Valid {
		value := odometer.Float64
		point.Odometer = &value
	}
	return point, nil
}

func parseNonNegativeInt64(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid non-negative integer")
	}
	return parsed, nil
}

func parsePageLimit(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 5000, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > 10000 {
		return 0, fmt.Errorf("limit must be between 1 and 10000")
	}
	return parsed, nil
}

func authorized(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		if tokenEqual(strings.TrimSpace(auth[7:]), apiToken) {
			return true
		}
	}
	if tokenEqual(auth, apiToken) {
		return true
	}
	if token := strings.TrimSpace(r.Header.Get("X-API-Token")); tokenEqual(token, apiToken) {
		return true
	}
	return authorizedByExistingAPI(r)
}

func tokenEqual(candidate, expected string) bool {
	if candidate == "" || expected == "" || len(candidate) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

// Reuse the authentication already configured for the user's TeslaMate API.
// The probe goes back through the local reverse proxy on a non-monitor endpoint,
// so Bearer, Basic, X-API-Token and Cloudflare Access modes keep one source of truth.
func authorizedByExistingAPI(r *http.Request) bool {
	if authProbeURL == "" {
		return false
	}

	probe, err := http.NewRequestWithContext(r.Context(), http.MethodGet, authProbeURL, nil)
	if err != nil {
		log.Printf("[warn] auth probe request: %v", err)
		return false
	}
	probe.Host = r.Host
	for _, name := range []string{
		"Authorization",
		"X-API-Token",
		"CF-Access-Client-Id",
		"CF-Access-Client-Secret",
	} {
		if value := r.Header.Get(name); value != "" {
			probe.Header.Set(name, value)
		}
	}

	response, err := authClient.Do(probe)
	if err != nil {
		log.Printf("[warn] auth probe failed: %v", err)
		return false
	}
	defer response.Body.Close()
	return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}

func fetchStates(carID int, startDate, endDate time.Time) (responseData, error) {
	const query = `
		SELECT
			s.state,
			s.start_date,
			s.end_date,
			c.name,
			start_sample.date,
			start_sample.battery_level,
			start_sample.rated_battery_range_km,
			end_before.date,
			end_before.battery_level,
			end_before.rated_battery_range_km,
			end_sample.date,
			end_sample.battery_level,
			end_sample.rated_battery_range_km
		FROM states s
		LEFT JOIN cars c ON c.id = s.car_id
		LEFT JOIN LATERAL (
			SELECT p.date, p.battery_level, p.rated_battery_range_km
			FROM positions p
			WHERE p.car_id = s.car_id
			  AND p.date <= s.start_date
			  AND p.date >= s.start_date - INTERVAL '30 minutes'
			  AND (p.battery_level IS NOT NULL OR p.rated_battery_range_km IS NOT NULL)
			ORDER BY p.date DESC
			LIMIT 1
		) start_sample ON TRUE
		LEFT JOIN LATERAL (
			SELECT p.date, p.battery_level, p.rated_battery_range_km
			FROM positions p
			WHERE p.car_id = s.car_id
			  AND s.end_date IS NOT NULL
			  AND p.date <= s.end_date
			  AND p.date >= s.end_date - INTERVAL '30 minutes'
			  AND (p.battery_level IS NOT NULL OR p.rated_battery_range_km IS NOT NULL)
			ORDER BY p.date DESC
			LIMIT 1
		) end_before ON TRUE
		LEFT JOIN LATERAL (
			SELECT p.date, p.battery_level, p.rated_battery_range_km
			FROM positions p
			WHERE p.car_id = s.car_id
			  AND s.end_date IS NOT NULL
			  AND p.date >= s.end_date
			  AND p.date <= s.end_date + INTERVAL '30 minutes'
			  AND (p.battery_level IS NOT NULL OR p.rated_battery_range_km IS NOT NULL)
			ORDER BY p.date ASC
			LIMIT 1
		) end_sample ON TRUE
		WHERE s.car_id = $1
		  AND s.start_date < $3
		  AND COALESCE(s.end_date, NOW()) > $2
		ORDER BY s.start_date ASC`

	rows, err := db.Query(query, carID, startDate.UTC(), endDate.UTC())
	if err != nil {
		return responseData{}, err
	}
	defer rows.Close()

	data := responseData{
		Car:    carRef{CarID: carID},
		States: []stateInterval{},
	}

	for rows.Next() {
		var (
			state            string
			start            time.Time
			end              sql.NullTime
			carName          sql.NullString
			startSampleDate  sql.NullTime
			startBattery     sql.NullInt64
			startRange       sql.NullFloat64
			endBeforeDate    sql.NullTime
			endBeforeBattery sql.NullInt64
			endBeforeRange   sql.NullFloat64
			endSampleDate    sql.NullTime
			endBattery       sql.NullInt64
			endRange         sql.NullFloat64
		)
		if err := rows.Scan(
			&state,
			&start,
			&end,
			&carName,
			&startSampleDate,
			&startBattery,
			&startRange,
			&endBeforeDate,
			&endBeforeBattery,
			&endBeforeRange,
			&endSampleDate,
			&endBattery,
			&endRange,
		); err != nil {
			return responseData{}, err
		}

		interval := stateInterval{
			State:     state,
			StartDate: start.In(location).Format(time.RFC3339),
		}
		if end.Valid {
			formatted := end.Time.In(location).Format(time.RFC3339)
			interval.EndDate = &formatted
		}
		interval.StartTelemetry = makeTelemetrySample(startSampleDate, startBattery, startRange)
		normalizedState := strings.ToLower(strings.TrimSpace(state))
		if normalizedState == "offline" || normalizedState == "asleep" {
			// The first observation at wake is the real post-sleep boundary.
			interval.EndTelemetry = makeTelemetrySample(endSampleDate, endBattery, endRange)
		} else {
			// For an online interval, do not jump across the following sleep.
			interval.EndTelemetry = makeTelemetrySample(endBeforeDate, endBeforeBattery, endBeforeRange)
		}

		data.States = append(data.States, interval)

		if data.Car.CarName == nil && carName.Valid && strings.TrimSpace(carName.String) != "" {
			name := carName.String
			data.Car.CarName = &name
		}
	}

	if err := rows.Err(); err != nil {
		return responseData{}, err
	}

	return data, nil
}

func makeTelemetrySample(date sql.NullTime, battery sql.NullInt64, ratedRange sql.NullFloat64) *telemetrySample {
	if !date.Valid {
		return nil
	}
	sample := telemetrySample{
		Date: date.Time.In(location).Format(time.RFC3339),
	}
	if battery.Valid {
		value := int(battery.Int64)
		sample.BatteryLevel = &value
	}
	if ratedRange.Valid {
		value := ratedRange.Float64
		sample.RatedRangeKM = &value
	}
	return &sample
}

func parseDateParam(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("missing date")
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported date: %s", trimmed)
}

func initDB() {
	host := getenv("DATABASE_HOST", "database")
	port := getenv("DATABASE_PORT", "5432")
	user := getenv("DATABASE_USER", "teslamate")
	pass := strings.TrimSpace(os.Getenv("DATABASE_PASS"))
	if pass == "" {
		log.Fatal("[error] DATABASE_PASS is required")
	}
	name := getenv("DATABASE_NAME", "teslamate")
	sslmode := getenv("DATABASE_SSL", "disable")
	timeout := getenv("DATABASE_TIMEOUT", "60000")

	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s connect_timeout=%s",
		host, port, user, pass, name, sslmode, timeout,
	)

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("[error] database open: %v", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("[error] database ping: %v", err)
	}
	log.Printf("[info] database connected")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
