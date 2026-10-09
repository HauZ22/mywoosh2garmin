// Command server runs the headless MyWhoosh/JOIN → Garmin sync daemon.
// It serves a small web UI plus HTTP/webhook endpoints for manual and
// automated (cron / script) activity sync.
//
// Configuration via environment variables:
//
//	HTTP_ADDR            listen address, default ":8080"
//	DATA_DIR             state directory (config, tokens, synced.json), default "./data"
//	GARMIN_EMAIL         Garmin Connect account
//	GARMIN_PASSWORD      Garmin Connect password (only needed for first login)
//	MYWHOOSH_EMAIL       MyWhoosh account
//	MYWHOOSH_PASSWORD    MyWhoosh password
//	SYNC_DAYS            default MyWhoosh look-back window, default 10
//	AUTH_USER            optional HTTP Basic Auth user (default "admin")
//	AUTH_PASSWORD        optional HTTP Basic Auth password; unset = no protection
//
// Credentials can alternatively be stored via the web UI; they are persisted
// to DATA_DIR/config.json.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mywhoosh2garmin/internal/core"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

//go:embed static/index.html
var staticFiles embed.FS

const (
	ringCapacity = 500
	maxUploadMB  = 64
)

// logRing is a bounded, thread-safe in-memory log.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRing) add(format string, args ...interface{}) {
	r.addLine(fmt.Sprintf(format, args...))
}

func (r *logRing) addLine(line string) {
	line = time.Now().Format("15:04:05 ") + line
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > ringCapacity {
		r.lines = r.lines[len(r.lines)-ringCapacity:]
	}
}

func (r *logRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || len(r.lines) < n {
		out := make([]string, len(r.lines))
		copy(out, r.lines)
		return out
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// server holds shared job state.
type server struct {
	syncer *core.Syncer
	log    *logRing

	// optional HTTP Basic Auth (empty password = disabled)
	authUser, authPass string

	cfgMu sync.Mutex // serialises read-modify-write of the configuration

	mu          sync.Mutex // guards the job state below
	busy        bool
	lastSummary core.Summary
	lastUpload  core.ItemResult
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the local server and exit 0 if healthy (for Docker HEALTHCHECK)")
	flag.Parse()

	addr := env("HTTP_ADDR", ":8080")

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *healthcheck {
		os.Exit(probe(addr))
	}

	dataDir := env("DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("DATA_DIR %q: %v", dataDir, err)
	}

	cfg := loadConfig(dataDir)
	ring := &logRing{}
	syncer := core.New(dataDir, cfg)
	syncer.SetLog(func(format string, args ...interface{}) {
		line := fmt.Sprintf(format, args...)
		log.Println(line)
		ring.addLine(line)
	})

	s := &server{
		syncer:   syncer,
		log:      ring,
		authUser: env("AUTH_USER", "admin"),
		authPass: os.Getenv("AUTH_PASSWORD"),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.autoSyncLoop(ctx)

	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.withAuth(mux),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authState := "ohne Zugriffsschutz"
	if s.authPass != "" {
		authState = "Basic-Auth aktiv"
	}
	log.Printf("MyWhoosh2Garmin %s läuft auf %s (Daten: %s, %s)", version, addr, dataDir, authState)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("Server beendet")
}

// loadConfig merges the persisted config with environment overrides (env wins).
func loadConfig(dataDir string) core.Config {
	cfg := core.LoadConfig(dataDir)
	if v := os.Getenv("GARMIN_EMAIL"); v != "" {
		cfg.GarminEmail = v
	}
	if v := os.Getenv("GARMIN_PASSWORD"); v != "" {
		cfg.GarminPassword = v
	}
	if v := os.Getenv("MYWHOOSH_EMAIL"); v != "" {
		cfg.MyWhooshEmail = v
	}
	if v := os.Getenv("MYWHOOSH_PASSWORD"); v != "" {
		cfg.MyWhooshPassword = v
	}
	if days, err := strconv.Atoi(os.Getenv("SYNC_DAYS")); err == nil && days > 0 {
		cfg.Days = days
	}
	if err := core.SaveConfig(dataDir, cfg); err != nil {
		log.Printf("warn: config speichern: %v", err)
	}
	return cfg
}

// probe is the Docker HEALTHCHECK: it requests /healthz on the local listener.
func probe(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func (s *server) routes(mux *http.ServeMux) {
	html, _ := staticFiles.ReadFile("static/index.html")
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/activities", s.handleActivities)
	mux.HandleFunc("POST /api/activities/mark", s.handleMarkActivities)
	mux.HandleFunc("POST /api/sync/activity", s.handleSyncActivity)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("POST /api/webhook/mywhoosh", s.handleSync)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("POST /api/webhook/upload", s.handleUpload)
}

// withAuth protects everything except /healthz with HTTP Basic Auth when
// AUTH_PASSWORD is set. Webhook callers can use `curl -u user:password`.
func (s *server) withAuth(next http.Handler) http.Handler {
	if s.authPass == "" {
		return next
	}
	wantUser := sha256.Sum256([]byte(s.authUser))
	wantPass := sha256.Sum256([]byte(s.authPass))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(user))
		gotPass := sha256.Sum256([]byte(pass))
		if !ok ||
			subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) != 1 ||
			subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="MyWhoosh2Garmin", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *server) respondJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) respondError(w http.ResponseWriter, code int, msg string) {
	s.respondJSON(w, code, map[string]string{"error": msg})
}

// statusConfig serialises the current configuration without plaintext secrets.
func (s *server) statusConfig() map[string]interface{} {
	cfg := s.syncer.Config()
	return map[string]interface{}{
		"mywhoosh_email":      cfg.MyWhooshEmail,
		"mywhoosh_configured": cfg.MyWhooshEmail != "" && cfg.MyWhooshPassword != "",
		"garmin_email":        cfg.GarminEmail,
		"garmin_configured":   cfg.GarminEmail != "" && cfg.GarminPassword != "",
		"days":                cfg.EffectiveDays(),
		"auto_sync_interval":  cfg.AutoSyncInterval,
	}
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	busy := s.busy
	lastSummary := s.lastSummary
	lastUpload := s.lastUpload
	s.mu.Unlock()

	var summaryResp interface{}
	if !(lastSummary.FinishedAt.IsZero() && lastSummary.StartedAt.IsZero()) {
		summaryResp = lastSummary
	}
	var uploadResp interface{}
	if lastUpload.Name != "" {
		uploadResp = lastUpload
	}

	s.respondJSON(w, http.StatusOK, map[string]interface{}{
		"version":      version,
		"busy":         busy,
		"config":       s.statusConfig(),
		"last_summary": summaryResp,
		"last_upload":  uploadResp,
		"log":          s.log.tail(2000),
	})
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MyWhooshEmail    string `json:"mywhoosh_email"`
		MyWhooshPassword string `json:"mywhoosh_password"`
		GarminEmail      string `json:"garmin_email"`
		GarminPassword   string `json:"garmin_password"`
		Days             int    `json:"days"`
		AutoSyncInterval *int   `json:"auto_sync_interval"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		s.respondError(w, http.StatusBadRequest, "ungültiger JSON-Body: "+err.Error())
		return
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	cfg := s.syncer.Config()
	if body.MyWhooshEmail != "" {
		cfg.MyWhooshEmail = body.MyWhooshEmail
	}
	if body.MyWhooshPassword != "" {
		cfg.MyWhooshPassword = body.MyWhooshPassword
	}
	if body.GarminEmail != "" {
		cfg.GarminEmail = body.GarminEmail
	}
	if body.GarminPassword != "" {
		cfg.GarminPassword = body.GarminPassword
	}
	if body.Days > 0 {
		cfg.Days = body.Days
	}
	if body.AutoSyncInterval != nil {
		interval := max(*body.AutoSyncInterval, 0)
		// Remember when auto-sync was switched on: older rides are never auto-uploaded.
		if cfg.AutoSyncInterval == 0 && interval > 0 {
			cfg.AutoSyncActivatedAt = time.Now().Unix()
		}
		cfg.AutoSyncInterval = interval
	}

	if err := s.syncer.SetConfig(cfg); err != nil {
		s.respondError(w, http.StatusInternalServerError, "Konfiguration speichern: "+err.Error())
		return
	}
	s.log.add("Konfiguration gespeichert (MyWhoosh: %s, Garmin: %s, Intervall: %d min)",
		cfg.MyWhooshEmail, cfg.GarminEmail, cfg.AutoSyncInterval)
	s.respondJSON(w, http.StatusOK, s.statusConfig())
}

func (s *server) handleActivities(w http.ResponseWriter, r *http.Request) {
	days := 0 // 0 = configured default
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
		days = d
	}

	items, err := s.syncer.ListActivities(days)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, items)
}

// handleMarkActivities marks activities as "already on Garmin" (synced=true) or
// removes the marker (synced=false). The marker is persisted in synced.json.
func (s *server) handleMarkActivities(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs    []string `json:"ids"`
		Synced bool     `json:"synced"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		s.respondError(w, http.StatusBadRequest, "ungültiger JSON-Body: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		s.respondError(w, http.StatusBadRequest, "ids fehlt")
		return
	}

	changed, err := s.syncer.SetActivitiesSynced(body.IDs, body.Synced)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError,
			"Markierung gesetzt, aber nicht dauerhaft gespeichert: "+err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, map[string]int{"changed": changed})
}

func (s *server) handleSyncActivity(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		s.respondError(w, http.StatusBadRequest, "ungültiger JSON-Body: "+err.Error())
		return
	}
	if body.ID == "" {
		s.respondError(w, http.StatusBadRequest, "id fehlt")
		return
	}

	if !s.tryLock() {
		s.respondError(w, http.StatusConflict, "ein Sync läuft bereits")
		return
	}
	go s.runSingleSync(body.ID)
	s.respondJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	if !s.tryLock() {
		s.respondError(w, http.StatusConflict, "ein Sync läuft bereits")
		return
	}
	go s.runSync(false)
	s.respondJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.tryLock() {
		s.respondError(w, http.StatusConflict, "ein Sync läuft bereits")
		return
	}

	name, data, err := readUpload(w, r)
	if err != nil {
		s.release()
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	go s.runUpload(name, data)
	s.respondJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// readUpload extracts an uploaded file either via multipart/form-data
// (field "file") or as a raw body with an X-Filename header.
func readUpload(w http.ResponseWriter, r *http.Request) (string, []byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadMB<<20)

	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return "", nil, fmt.Errorf("Multipart-Parsing: %w", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			return "", nil, fmt.Errorf("kein File-Attribut 'file' im Upload: %w", err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return "", nil, err
		}
		return filepath.Base(header.Filename), data, nil
	}

	name := r.Header.Get("X-Filename")
	if name == "" {
		return "", nil, fmt.Errorf("raw body benötigt X-Filename-Header (oder multipart 'file')")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, fmt.Errorf("Datei zu groß oder unlesbar (max. %d MB): %w", maxUploadMB, err)
	}
	return filepath.Base(name), data, nil
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

func (s *server) tryLock() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *server) release() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

// autoSyncLoop starts a sync every configured interval (minutes) until ctx ends.
// It takes the same busy lock as manual jobs, so runs never overlap.
func (s *server) autoSyncLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var lastSync time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		interval := s.syncer.Config().AutoSyncInterval
		if interval <= 0 || time.Since(lastSync) < time.Duration(interval)*time.Minute {
			continue
		}
		if !s.tryLock() {
			continue // a manual job is running; try again next tick
		}
		lastSync = time.Now()
		s.runSync(true)
	}
}

// runSync runs a full MyWhoosh sync. The caller must have acquired the busy lock.
func (s *server) runSync(auto bool) {
	defer s.release()
	label := "MyWhoosh-Sync"
	if auto {
		label = "Automatischer Sync"
	}
	s.log.add("▶ %s gestartet", label)
	summary, _ := s.syncer.SyncMyWhoosh(0, auto)
	s.mu.Lock()
	s.lastSummary = summary
	s.mu.Unlock()
	s.log.add("✔ %s beendet (%d hoch, %d Duplikate, %d fehlgeschlagen)",
		label, summary.Uploaded, summary.Duplicates, summary.Failed)
}

// runSingleSync uploads one activity. The caller must have acquired the busy lock.
func (s *server) runSingleSync(id string) {
	defer s.release()
	s.log.add("▶ Einzel-Sync gestartet für Aktivität: %s", id)
	item, err := s.syncer.SyncActivityByID(id)
	s.mu.Lock()
	s.lastUpload = item
	s.mu.Unlock()

	switch {
	case err != nil:
		s.log.add("✖ Aktivität %s fehlgeschlagen: %v", id, err)
	case item.Status == core.StatusUploaded:
		s.log.add("✔ Aktivität %s erfolgreich hochgeladen", id)
	case item.Status == core.StatusDuplicate:
		s.log.add("⚠ Aktivität %s war bereits auf Garmin (als hochgeladen gemerkt)", id)
	default:
		s.log.add("✔ Aktivität %s übersprungen: %s", id, item.Message)
	}
}

// runUpload patches and uploads a file. The caller must have acquired the busy lock.
func (s *server) runUpload(name string, data []byte) {
	defer s.release()
	s.log.add("▶ Datei-Upload gestartet: %s", name)
	item, _ := s.syncer.UploadFileBytes(name, data)
	s.mu.Lock()
	s.lastUpload = item
	s.mu.Unlock()

	switch item.Status {
	case core.StatusUploaded:
		s.log.add("✔ %s hochgeladen", name)
	case core.StatusDuplicate:
		s.log.add("⚠ %s: Duplikat (übersprungen)", name)
	default:
		s.log.add("✖ %s fehlgeschlagen: %s", name, item.Message)
	}
}
