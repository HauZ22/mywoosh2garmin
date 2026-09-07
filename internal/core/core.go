// Package core provides the shared activity-sync pipeline used by both the
// desktop GUI and the headless web server: MyWhoosh activity sync, Garmin
// authentication and generic FIT/TCX file upload with persistent state
// (config, sync history and Garmin/MyWhoosh session tokens).
package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mywhoosh2garmin/garmin"
	"mywhoosh2garmin/internal/fitfix"
	"mywhoosh2garmin/internal/tcx"
	"mywhoosh2garmin/mywhoosh"
)

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// LogFn receives format-style log lines.
type LogFn func(format string, args ...interface{})

func noopLog(string, ...interface{}) {}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config holds the account credentials and sync preferences. The password
// fields are optional on the desktop where Garmin only needs one interactive
// login; the server persists them so it can authenticate headlessly.
type Config struct {
	MyWhooshEmail    string `json:"mywhoosh_email,omitempty"`
	MyWhooshPassword string `json:"mywhoosh_password,omitempty"`
	GarminEmail      string `json:"garmin_email,omitempty"`
	GarminPassword   string `json:"garmin_password,omitempty"`
	Days             int    `json:"days,omitempty"`
	AutoSyncInterval int    `json:"auto_sync_interval,omitempty"`
	AutoSyncActivatedAt int64 `json:"auto_sync_activated_at,omitempty"`
}

// DefaultDays is the activity look-back window used when Days is unset.
const DefaultDays = 10

// EffectiveDays returns the configured look-back window or DefaultDays.
func (c *Config) EffectiveDays() int {
	if c.Days <= 0 {
		return DefaultDays
	}
	return c.Days
}

// LoadConfig reads config.json from dir. Missing or invalid files yield an
// empty Config, never an error.
func LoadConfig(dir string) Config {
	var cfg Config
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	return cfg
}

// SaveConfig writes config.json to dir.
func SaveConfig(dir string, cfg Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600)
}

// ---------------------------------------------------------------------------
// Synced-activity tracker
// ---------------------------------------------------------------------------

// SyncedTracker persists the set of already-uploaded activity keys so the
// same training is never uploaded twice.
type SyncedTracker struct {
	mu       sync.Mutex
	path     string
	uploaded map[string]string // key → upload timestamp
}

// NewSyncedTracker loads (or initialises) the tracker stored in dir/synced.json.
func NewSyncedTracker(dir string) *SyncedTracker {
	st := &SyncedTracker{
		path:     filepath.Join(dir, "synced.json"),
		uploaded: make(map[string]string),
	}
	data, err := os.ReadFile(st.path)
	if err == nil {
		_ = json.Unmarshal(data, &st.uploaded)
	}
	return st
}

// IsSynced reports whether the given activity key was already uploaded.
func (st *SyncedTracker) IsSynced(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.uploaded[key]
	return ok
}

// MarkSynced records the given activity key as uploaded.
func (st *SyncedTracker) MarkSynced(key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.uploaded[key] = time.Now().Format(time.RFC3339)
	data, _ := json.MarshalIndent(st.uploaded, "", "  ")
	_ = os.WriteFile(st.path, data, 0o600)
}

// ---------------------------------------------------------------------------
// Sync results
// ---------------------------------------------------------------------------

// ActivityDisplayItem describes a MyWhoosh activity for display in the UI.
type ActivityDisplayItem struct {
	mywhoosh.Activity
	IsSynced bool `json:"is_synced"`
}

// ItemResult describes the outcome for one activity.
type ItemResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"` // uploaded | duplicate | failed | skipped
	Message string `json:"message,omitempty"`
}

// Summary describes the outcome of a sync run.
type Summary struct {
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Total      int          `json:"total"`
	Uploaded   int          `json:"uploaded"`
	Duplicates int          `json:"duplicates"`
	Failed     int          `json:"failed"`
	Skipped    int          `json:"skipped"`
	Items      []ItemResult `json:"items"`
	Error      string       `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

// Syncer encapsulates the Garmin + MyWhoosh clients plus persistent state
// (tokens, sync tracker) for a single data directory.
type Syncer struct {
	dataDir string
	cfg     Config
	log     LogFn

	tracker *SyncedTracker
	garmin  *garmin.Client
	mw      *mywhoosh.Client
}

// New creates a Syncer rooted at dataDir (tokens, config.json, synced.json).
func New(dataDir string, cfg Config) *Syncer {
	return &Syncer{
		dataDir: dataDir,
		cfg:     cfg,
		log:     noopLog,
		tracker: NewSyncedTracker(dataDir),
	}
}

// SetLog sets the sync log sink.
func (s *Syncer) SetLog(fn LogFn) {
	if fn != nil {
		s.log = fn
	}
}

// DataDir returns the persistent state directory.
func (s *Syncer) DataDir() string { return s.dataDir }

// Config returns a copy of the current configuration.
func (s *Syncer) Config() Config { return s.cfg }

// SetConfig replaces the configuration and persists it.
func (s *Syncer) SetConfig(cfg Config) error {
	s.cfg = cfg
	return SaveConfig(s.dataDir, cfg)
}

// Tracker exposes the synced-activity tracker (for the desktop UI).
func (s *Syncer) Tracker() *SyncedTracker { return s.tracker }

// GetMyWhooshActivitiesForDisplay fetches outstanding MyWhoosh activities from
// the last N days and returns them for display in the UI with their sync status.
func (s *Syncer) GetMyWhooshActivitiesForDisplay(days int) ([]ActivityDisplayItem, error) {
	if days <= 0 {
		days = s.cfg.EffectiveDays()
	}

	if err := s.ensureMyWhoosh(); err != nil {
		return nil, err
	}
	s.log("MyWhoosh Aktivitäten (letzte %d Tage) abrufen…", days)
	activities, err := s.mw.GetRecentActivities(days)
	if err != nil {
		s.log("  ❌ Token evtl. abgelaufen, neuer Login…")
		if loginErr := s.mw.Login(s.cfg.MyWhooshEmail, s.cfg.MyWhooshPassword); loginErr == nil {
			activities, err = s.mw.GetRecentActivities(days)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("Aktivitäten abrufen fehlgeschlagen: %w", err)
	}

	var displayItems []ActivityDisplayItem
	for _, act := range activities {
		displayItems = append(displayItems, ActivityDisplayItem{
			Activity: act,
			IsSynced: s.tracker.IsSynced(act.ID),
		})
	}
	return displayItems, nil
}

// SyncSingleMyWhooshActivity downloads, fixes, and uploads a single MyWhoosh activity.
func (s *Syncer) SyncSingleMyWhooshActivity(activityID string) (ItemResult, error) {
	if err := s.ensureMyWhoosh(); err != nil {
		return ItemResult{ID: activityID, Status: "failed", Message: err.Error()}, err
	}

	// Find the activity by ID (we might have fetched more than requested days originally)
	// For now, let's refetch recent activities to ensure we have it.
	// A more efficient way would be to get all activities and then filter, but for now this is simpler.
	activities, err := s.mw.GetRecentActivities(s.cfg.EffectiveDays())
	if err != nil {
		return ItemResult{ID: activityID, Status: "failed", Message: fmt.Sprintf("Aktivitäten abrufen fehlgeschlagen: %v", err)}, err
	}

	var targetActivity *mywhoosh.Activity
	for _, act := range activities {
		if act.ID == activityID {
			targetActivity = &act
			break
		}
	}

	if targetActivity == nil {
		return ItemResult{ID: activityID, Status: "failed", Message: "Aktivität nicht gefunden"}, fmt.Errorf("activity %s not found", activityID)
	}

	return s.processAndUploadMyWhooshActivity(*targetActivity, false)
}

func (s *Syncer) processAndUploadMyWhooshActivity(act mywhoosh.Activity, isAutoSync bool) (ItemResult, error) {
	item := ItemResult{ID: act.ID, Name: act.DisplayName()}

	if s.tracker.IsSynced(act.ID) {
		item.Status = "skipped"
		item.Message = "Bereits synchronisiert"
		return item, nil
	}
    
    // Safety check for auto-sync: skip activities before activation
    if isAutoSync && s.cfg.AutoSyncActivatedAt > 0 && int64(act.Date) < s.cfg.AutoSyncActivatedAt {
        s.log("  [Auto-Sync] Überspringe alte Aktivität (vor Aktivierung): %s", act.DisplayName())
        s.tracker.MarkSynced(act.ID) // Mark as synced silently
        item.Status = "skipped"
        item.Message = "Vor Auto-Sync Aktivierung"
        return item, nil
    }

	s.log("▶ %s (%s)", act.DisplayName(), act.FormattedDate())

	// Download FIT from MyWhoosh
	fileID := act.ActivityFileID
	if fileID == "" {
		fileID = act.ID
	}
	fitData, err := s.mw.DownloadFitFile(fileID)
	if err != nil {
		item.Status, item.Message = "failed", "Download fehlgeschlagen: "+err.Error()
		return item, err
	}
	s.log("  ✓ %d Bytes heruntergeladen", len(fitData))

	// Fix the FIT file (in memory)
	fixed, err := fitfix.FixFit(fitData)
	if err != nil {
		item.Status, item.Message = "failed", "FIT-Fix fehlgeschlagen: "+err.Error()
		return item, err
	}

	// Authenticate to Garmin
	if err := s.EnsureGarmin(); err != nil {
		item.Status, item.Message = "failed", "Garmin-Authentifizierung fehlgeschlagen: "+err.Error()
		return item, err
	}

	// Upload to Garmin
	path, err := writeTempFile("mw_"+act.ID+".fit", fixed)
	if err != nil {
		item.Status, item.Message = "failed", "Temp-Datei erstellen fehlgeschlagen: "+err.Error()
		return item, err
	}
	defer os.Remove(path)

	s.log("  ⬆ Upload zu Garmin Connect…")
	if err := s.garmin.UploadFIT(path); err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			s.log("  ⚠ Bereits auf Garmin (Duplikat)")
			item.Status = "duplicate"
			s.tracker.MarkSynced(act.ID)
		} else {
			item.Status, item.Message = "failed", "Upload fehlgeschlagen: "+err.Error()
		}
	} else {
		s.log("  ✓ Hochgeladen")
		item.Status = "uploaded"
		s.tracker.MarkSynced(act.ID)
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// Garmin authentication
// ---------------------------------------------------------------------------

// EnsureGarmin authenticates to Garmin Connect, resuming a cached session,
// refreshing the OAuth2 token, or performing a fresh login when credentials
// are configured. A valid session is cached in dataDir for reuse.
func (s *Syncer) EnsureGarmin() error {
	if s.garmin != nil && s.garmin.OAuth2 != nil && !s.garmin.OAuth2.Expired() {
		return nil
	}

	client := garmin.NewClient(s.dataDir)
	if err := client.Resume(); err == nil {
		s.garmin = client
		return nil
	}

	email, password := s.cfg.GarminEmail, s.cfg.GarminPassword
	if email == "" || password == "" {
		return fmt.Errorf("Garmin ist nicht eingerichtet — E-Mail & Passwort erforderlich")
	}

	s.log("Garmin Login…")
	if err := client.Login(email, password); err != nil {
		return fmt.Errorf("Garmin login failed: %w", err)
	}
	s.garmin = client
	return nil
}

// ensureMyWhoosh logs in to MyWhoosh, resuming a cached session if possible.
func (s *Syncer) ensureMyWhoosh() error {
	if s.mw == nil {
		s.mw = mywhoosh.NewClient(s.dataDir)
	}
	if s.mw.Resume() == nil {
		return nil
	}

	email, password := s.cfg.MyWhooshEmail, s.cfg.MyWhooshPassword
	if email == "" || password == "" {
		return fmt.Errorf("MyWhoosh ist nicht eingerichtet — E-Mail & Passwort erforderlich")
	}
	s.log("MyWhoosh Login…")
	if err := s.mw.Login(email, password); err != nil {
		return fmt.Errorf("MyWhoosh login failed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MyWhoosh activity sync
// ---------------------------------------------------------------------------

// SyncMyWhoosh fetches outstanding MyWhoosh activities from the last N days,
// fixes their FIT files and uploads them to Garmin Connect.
func (s *Syncer) SyncMyWhoosh(days int, isAutoSync bool) (Summary, error) {
	if days <= 0 {
		days = s.cfg.EffectiveDays()
	}
	summary := Summary{StartedAt: time.Now()}

	if err := s.ensureMyWhoosh(); err != nil {
		summary.FinishedAt = time.Now()
		summary.Error = err.Error()
		return summary, err
	}
	s.log("MyWhoosh Aktivitäten (letzte %d Tage) abrufen…", days)
	activities, err := s.mw.GetRecentActivities(days)
	if err != nil {
		s.log("  ❌ Token evtl. abgelaufen, neuer Login…")
		if loginErr := s.mw.Login(s.cfg.MyWhooshEmail, s.cfg.MyWhooshPassword); loginErr == nil {
			activities, err = s.mw.GetRecentActivities(days)
		}
	}
	if err != nil {
		summary.FinishedAt = time.Now()
		summary.Error = fmt.Sprintf("Aktivitäten abrufen fehlgeschlagen: %v", err)
		return summary, err
	}

	summary.Total = len(activities)
	if summary.Total == 0 {
		summary.FinishedAt = time.Now()
		s.log("Keine Aktivitäten in den letzten %d Tagen.", days)
		return summary, nil
	}
	s.log("✓ %d Aktivitäten gefunden", summary.Total)

	for i, act := range activities {
		s.log("[%d/%d] %s", i+1, summary.Total, act.DisplayName())
		item, err := s.processAndUploadMyWhooshActivity(act, isAutoSync)
		if err != nil && item.Status != "duplicate" {
			summary.Failed++
		} else if item.Status == "duplicate" {
			summary.Duplicates++
		} else if item.Status == "uploaded" {
			summary.Uploaded++
		}
		summary.Items = append(summary.Items, item)
	}

	summary.FinishedAt = time.Now()
	s.log("✓ Sync abgeschlossen — %d hochgeladen, %d Duplikate, %d fehlgeschlagen",
		summary.Uploaded, summary.Duplicates, summary.Failed)
	return summary, nil
}

// ---------------------------------------------------------------------------
// Generic file upload (FIT or TCX)
// ---------------------------------------------------------------------------

// UploadFileBytes patches and uploads a single activity file. name must carry
// a .fit or .tcx extension. Returns the item status, message and dedup key.
func (s *Syncer) UploadFileBytes(name string, data []byte) (ItemResult, error) {
	lower := strings.ToLower(name)
	item := ItemResult{Name: name}

	hash := tcx.ContentHash(data)
	if strings.HasSuffix(lower, ".tcx") || strings.HasSuffix(lower, ".xml") {
		if s.tracker.IsSynced(hash) {
			item.Status = "duplicate"
			return item, nil
		}
		fixed, err := tcx.FixTcx(data)
		if err != nil {
			return ItemResult{Name: name, Status: "failed", Message: "TCX-Fix: " + err.Error()}, err
		}
		return s.uploadPatched(name, fixed, &item, hash)
	}

	if strings.HasSuffix(lower, ".fit") {
		if s.tracker.IsSynced(hash) {
			item.Status = "duplicate"
			return item, nil
		}
		fixed, err := fitfix.FixFit(data)
		if err != nil {
			return ItemResult{Name: name, Status: "failed", Message: "FIT-Fix: " + err.Error()}, err
		}
		return s.uploadPatched(name, fixed, &item, hash)
	}

	return ItemResult{Name: name, Status: "failed", Message: "Unbekanntes Format (nutze .fit oder .tcx)"},
		fmt.Errorf("unsupported file format: %q", name)
}

func (s *Syncer) uploadPatched(tmpName string, data []byte, item *ItemResult, key string) (ItemResult, error) {
	if err := s.EnsureGarmin(); err != nil {
		return ItemResult{Name: item.Name, Status: "failed", Message: err.Error()}, err
	}

	path, err := writeTempFile(tmpName, data)
	if err != nil {
		return ItemResult{Name: item.Name, Status: "failed", Message: "Temp: " + err.Error()}, err
	}
	defer os.Remove(path)

	s.log("  ⬆ Upload %s…", tmpName)
	if err := s.garmin.UploadFile(path); err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			s.log("  ⚠ Bereits auf Garmin (duplicate)")
			item.Status = "duplicate"
			s.tracker.MarkSynced(key)
			return *item, nil
		}
		item.Status = "failed"
		item.Message = err.Error()
		return *item, err
	}
	s.log("  ✓ Hochgeladen")
	item.Status = "uploaded"
	s.tracker.MarkSynced(key)
	return *item, nil
}

// writeTempFile stores bytes in a temp file for the Garmin multipart upload.
// The random suffix is inserted before the file extension so the uploaded
// file keeps a valid .fit/.tcx extension that Garmin can detect.
func writeTempFile(name string, data []byte) (string, error) {
	f, err := os.CreateTemp("", "fittogarmin-*"+filepath.Ext(name))
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}