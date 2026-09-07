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
//
// Credentials can alternatively be stored via the web UI; they are persisted
// to DATA_DIR/config.json.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mywhoosh2garmin/internal/core"
)

//go:embed static/index.html
var staticFiles embed.FS

const ringCapacity = 500

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
	dataDir string
	cfg     core.Config
	syncer  *core.Syncer
	log     *logRing

	mu          sync.Mutex
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
	dataDir := env("DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("DATA_DIR %q: %v", dataDir, err)
	}

	// Merge persisted config with environment overrides (env wins).
	cfg := core.LoadConfig(dataDir)
	if v := os.Getenv("GARMIN_EMAIL"); v != "" {
		cfg.GarminEmail = v
		cfg.GarminPassword = os.Getenv("GARMIN_PASSWORD")
	}
	if v := os.Getenv("MYWHOOSH_EMAIL"); v != "" {
		cfg.MyWhooshEmail = v
		cfg.MyWhooshPassword = os.Getenv("MYWHOOSH_PASSWORD")
	}
	if v := os.Getenv("SYNC_DAYS"); v != "" {
		var days int
		if _, err := fmt.Sscanf(v, "%d", &days); err == nil && days > 0 {
			cfg.Days = days
		}
	}
	if err := core.SaveConfig(dataDir, cfg); err != nil {
		log.Printf("warn: config speichern: %v", err)
	}

	ring := &logRing{}
	syncer := core.New(dataDir, cfg)
	syncer.SetLog(func(format string, args ...interface{}) {
		line := fmt.Sprintf(format, args...)
		log.Println(line)
		ring.addLine(line)
	})

	s := &server{dataDir: dataDir, cfg: cfg, syncer: syncer, log: ring}

	// Auto-sync ticker
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		var lastSync time.Time
		for range ticker.C {
			if s.cfg.AutoSyncInterval <= 0 {
				continue
			}
			
			s.mu.Lock()
			if s.busy || time.Since(lastSync) < time.Duration(s.cfg.AutoSyncInterval)*time.Minute {
				s.mu.Unlock()
				continue
			}
			s.mu.Unlock()

			s.log.add("▶ Automatischer Sync gestartet")
			lastSync = time.Now()
			
			summary, _ := s.syncer.SyncMyWhoosh(s.cfg.EffectiveDays(), true)
			
			s.mu.Lock()
			s.lastSummary = summary
			s.mu.Unlock()
			s.log.add("✔ Automatischer Sync beendet (%d hoch, %d Duplikate, %d fehlgeschlagen)",
				summary.Uploaded, summary.Duplicates, summary.Failed)
		}
	}()

	addr := env("HTTP_ADDR", ":8080")
	log.Printf("FIT → Garmin Sync läuft auf %s (Daten: %s)", addr, dataDir)

	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
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

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/activities", s.handleActivities)
	mux.HandleFunc("POST /api/sync/activity", s.handleSyncActivity)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("POST /api/webhook/mywhoosh", s.handleSync)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("POST /api/webhook/upload", s.handleUpload)
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

// statusSnapshot serialises the current configuration without plaintext secrets.
func (s *server) statusConfig() map[string]interface{} {
	return map[string]interface{}{
		"mywhoosh_email":        s.cfg.MyWhooshEmail,
		"mywhoosh_configured":   s.cfg.MyWhooshEmail != "" && s.cfg.MyWhooshPassword != "",
		"garmin_email":          s.cfg.GarminEmail,
		"garmin_configured":     s.cfg.GarminEmail != "" && s.cfg.GarminPassword != "",
		"days":                  s.cfg.EffectiveDays(),
		"auto_sync_interval":    s.cfg.AutoSyncInterval,
	}
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	busy := s.busy
	lastSummary := s.lastSummary
	lastUpload := s.lastUpload
	s.mu.Unlock()

	var summaryResp interface{}
	if lastSummary.FinishedAt.IsZero() && lastSummary.Total == 0 && lastSummary.Items == nil && !busy {
		summaryResp = nil
	} else {
		summaryResp = lastSummary
	}
	var uploadResp interface{}
	if lastUpload.Name == "" {
		uploadResp = nil
	} else {
		uploadResp = lastUpload
	}

	resp := map[string]interface{}{
		"busy":         busy,
		"config":       s.statusConfig(),
		"last_summary": summaryResp,
		"last_upload":  uploadResp,
		"log":          s.log.tail(2000),
	}
	s.respondJSON(w, http.StatusOK, resp)
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MyWhooshEmail    string `json:"mywhoosh_email"`
		MyWhooshPassword string `json:"mywhoosh_password"`
		GarminEmail      string `json:"garmin_email"`
		GarminPassword   string `json:"garmin_password"`
		Days             int    `json:"days"`
		AutoSyncInterval int    `json:"auto_sync_interval"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		s.respondError(w, http.StatusBadRequest, "ungültiger JSON-Body: "+err.Error())
		return
	}

	cfg := s.cfg
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
	// If auto-sync was disabled (0) and is now enabled (>0), set activation time
	if cfg.AutoSyncInterval == 0 && body.AutoSyncInterval > 0 {
		cfg.AutoSyncActivatedAt = time.Now().Unix()
	}
	cfg.AutoSyncInterval = body.AutoSyncInterval
	
	if err := s.syncer.SetConfig(cfg); err != nil {
		s.respondError(w, http.StatusInternalServerError, "Konfiguration speichern: "+err.Error())
		return
	}
	s.cfg = cfg
	s.log.add("Konfiguration gespeichert (MyWhoosh: %s, Garmin: %s, Intervall: %d min)", cfg.MyWhooshEmail, cfg.GarminEmail, cfg.AutoSyncInterval)
	s.respondJSON(w, http.StatusOK, s.statusConfig())
}
func (s *server) handleActivities(w http.ResponseWriter, r *http.Request) {
	days := s.cfg.EffectiveDays()
	if q := r.URL.Query().Get("days"); q != "" {
		var d int
		if _, err := fmt.Sscanf(q, "%d", &d); err == nil && d > 0 {
			days = d
		}
	}

	items, err := s.syncer.GetMyWhooshActivitiesForDisplay(days)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, items)
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

	go func() {
		defer s.release()
		s.log.add("▶ Einzel-Sync gestartet für Aktivität: %s", body.ID)
		item, err := s.syncer.SyncSingleMyWhooshActivity(body.ID)
		s.mu.Lock()
		s.lastUpload = item
		s.mu.Unlock()
		if err != nil {
			s.log.add("✖ Aktivität %s fehlgeschlagen: %v", body.ID, err)
			return
		}
		if item.Status == "uploaded" {
			s.log.add("✔ Aktivität %s erfolgreich hochgeladen", body.ID)
		} else if item.Status == "duplicate" {
			s.log.add("⚠ Aktivität %s ist ein Duplikat (übersprungen)", body.ID)
		} else if item.Status == "skipped" {
			s.log.add("✔ Aktivität %s bereits synchronisiert", body.ID)
		} else {
			s.log.add("✖ Aktivität %s fehlgeschlagen: %s", body.ID, item.Message)
		}
	}()

	s.respondJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	if !s.tryLock() {
		s.respondError(w, http.StatusConflict, "ein Sync läuft bereits")
		return
	}
	go s.runSync()
	s.respondJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.tryLock() {
		s.respondError(w, http.StatusConflict, "ein Sync läuft bereits")
		return
	}

	name, data, err := readUpload(r)
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
func readUpload(r *http.Request) (string, []byte, error) {
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return "", nil, fmt.Errorf("Multipart-Parsing: %w", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			return "", nil, fmt.Errorf("kein File-Attribut 'file' im Upload: %w", err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 64<<20))
		if err != nil {
			return "", nil, err
		}
		return filepath.Base(header.Filename), data, nil
	}

	name := r.Header.Get("X-Filename")
	if name == "" {
		return "", nil, fmt.Errorf("raw body benötigt X-Filename-Header (oder multipart 'file')")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return "", nil, err
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

func (s *server) runSync() {
	defer s.release()
	s.log.add("▶ MyWhoosh-Sync gestartet")
	summary, _ := s.syncer.SyncMyWhoosh(s.cfg.EffectiveDays(), false)
	s.mu.Lock()
	s.lastSummary = summary
	s.mu.Unlock()
	s.log.add("✔ MyWhoosh-Sync beendet (%d hoch, %d Duplikate, %d fehlgeschlagen)",
		summary.Uploaded, summary.Duplicates, summary.Failed)
}

func (s *server) runUpload(name string, data []byte) {
	defer s.release()
	s.log.add("▶ Datei-Upload gestartet: %s", name)
	item, _ := s.syncer.UploadFileBytes(name, data)
	s.mu.Lock()
	s.lastUpload = item
	s.mu.Unlock()
	if item.Status == "uploaded" {
		s.log.add("✔ %s hochgeladen", name)
	} else if item.Status == "duplicate" {
		s.log.add("⚠ %s: Duplikat (übersprungen)", name)
	} else {
		s.log.add("✖ %s fehlgeschlagen: %s", name, item.Message)
	}
}