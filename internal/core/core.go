// Package core provides the shared activity-sync pipeline used by both the
// desktop GUI and the headless web server: MyWhoosh activity sync, Garmin
// authentication and generic FIT/TCX file upload with persistent state
// (config, sync history and Garmin/MyWhoosh session tokens).
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// LogFn receives format-style log lines (without trailing newline).
type LogFn func(format string, args ...interface{})

func noopLog(string, ...interface{}) {}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config holds the account credentials and sync preferences. The password
// fields are optional on the desktop where Garmin only needs one interactive
// login; the server persists them so it can authenticate headlessly.
type Config struct {
	MyWhooshEmail       string `json:"mywhoosh_email,omitempty"`
	MyWhooshPassword    string `json:"mywhoosh_password,omitempty"`
	GarminEmail         string `json:"garmin_email,omitempty"`
	GarminPassword      string `json:"garmin_password,omitempty"`
	Days                int    `json:"days,omitempty"`
	AutoSyncInterval    int    `json:"auto_sync_interval,omitempty"`
	AutoSyncActivatedAt int64  `json:"auto_sync_activated_at,omitempty"`
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
// Results
// ---------------------------------------------------------------------------

// Item status values.
const (
	StatusUploaded  = "uploaded"
	StatusDuplicate = "duplicate"
	StatusSkipped   = "skipped"
	StatusFailed    = "failed"
)

// ActivityDisplayItem describes a MyWhoosh activity for display in the UI.
type ActivityDisplayItem struct {
	mywhoosh.Activity
	IsSynced   bool       `json:"is_synced"`
	SyncSource SyncSource `json:"sync_source,omitempty"`
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
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

func (sum *Summary) add(item ItemResult) {
	sum.Items = append(sum.Items, item)
	switch item.Status {
	case StatusUploaded:
		sum.Uploaded++
	case StatusDuplicate:
		sum.Duplicates++
	case StatusSkipped:
		sum.Skipped++
	default:
		sum.Failed++
	}
}

// ErrGarminAuth marks a failed Garmin login. A sync run stops at the first one
// instead of retrying the login for every remaining activity.
var ErrGarminAuth = errors.New("garmin authentication failed")

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

// Syncer encapsulates the Garmin + MyWhoosh clients plus persistent state
// (tokens, sync tracker) for a single data directory. It is safe for
// concurrent use: network operations are serialised internally.
type Syncer struct {
	dataDir string
	log     LogFn
	tracker *SyncedTracker

	mu          sync.RWMutex // guards cfg and the stale flags
	cfg         Config
	mwStale     bool // MyWhoosh account changed → drop cached session
	garminStale bool

	opMu   sync.Mutex // serialises operations that talk to MyWhoosh/Garmin
	garmin *garmin.Client
	mw     *mywhoosh.Client

	knownMu sync.Mutex
	known   map[string]mywhoosh.Activity // last fetched activities by ID
}

// New creates a Syncer rooted at dataDir (tokens, config.json, synced.json).
func New(dataDir string, cfg Config) *Syncer {
	return &Syncer{
		dataDir: dataDir,
		cfg:     cfg,
		log:     noopLog,
		tracker: NewSyncedTracker(dataDir),
		known:   make(map[string]mywhoosh.Activity),
	}
}

// SetLog sets the sync log sink. Call it before starting any operation.
func (s *Syncer) SetLog(fn LogFn) {
	if fn != nil {
		s.log = fn
	}
}

// DataDir returns the persistent state directory.
func (s *Syncer) DataDir() string { return s.dataDir }

// Config returns a copy of the current configuration.
func (s *Syncer) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// ApplyConfig replaces the in-memory configuration without persisting it
// (the desktop app uses this to keep passwords out of config.json). Cached
// sessions are dropped when an account's e-mail changes.
func (s *Syncer) ApplyConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.MyWhooshEmail != "" && cfg.MyWhooshEmail != s.cfg.MyWhooshEmail {
		s.mwStale = true
	}
	if s.cfg.GarminEmail != "" && cfg.GarminEmail != s.cfg.GarminEmail {
		s.garminStale = true
	}
	s.cfg = cfg
}

// SetConfig replaces the configuration and persists it to config.json.
func (s *Syncer) SetConfig(cfg Config) error {
	s.ApplyConfig(cfg)
	return SaveConfig(s.dataDir, cfg)
}

// Tracker exposes the synced-activity tracker.
func (s *Syncer) Tracker() *SyncedTracker { return s.tracker }

// takeStale reads and clears a stale flag.
func (s *Syncer) takeStale(flag *bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := *flag
	*flag = false
	return v
}

// ---------------------------------------------------------------------------
// Activity listing and manual marking
// ---------------------------------------------------------------------------

// ListActivities fetches the MyWhoosh activities from the last N days (0 =
// configured default) together with their sync status.
func (s *Syncer) ListActivities(days int) ([]ActivityDisplayItem, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	activities, err := s.fetchActivities(days)
	if err != nil {
		return nil, err
	}

	items := make([]ActivityDisplayItem, 0, len(activities))
	for _, act := range activities {
		item := ActivityDisplayItem{Activity: act}
		if e, ok := s.tracker.Entry(act.ID); ok {
			item.IsSynced = true
			item.SyncSource = e.Source
			if !e.At.IsZero() {
				at := e.At
				item.SyncedAt = &at
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// SetActivitiesSynced marks MyWhoosh activities as already uploaded to Garmin
// (synced=true), or removes that marker again (synced=false). The marker is
// stored permanently in synced.json. Returns how many activities changed.
func (s *Syncer) SetActivitiesSynced(ids []string, synced bool) (int, error) {
	changed := 0
	var firstErr error
	for _, id := range ids {
		if id == "" || s.tracker.IsSynced(id) == synced {
			continue
		}
		var err error
		if synced {
			err = s.tracker.Mark(id, SourceManual, s.knownName(id))
		} else {
			err = s.tracker.Unmark(id)
		}
		changed++
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if changed > 0 {
		if synced {
			s.log("✔ %d Aktivität(en) manuell als hochgeladen markiert", changed)
		} else {
			s.log("↩ Markierung bei %d Aktivität(en) entfernt", changed)
		}
	}
	return changed, firstErr
}

func (s *Syncer) knownName(id string) string {
	s.knownMu.Lock()
	defer s.knownMu.Unlock()
	if act, ok := s.known[id]; ok {
		return act.DisplayName()
	}
	return ""
}

// mark records an activity as synced and logs if the history can't be saved —
// a lost marker would otherwise lead to a repeated upload.
func (s *Syncer) mark(key string, source SyncSource, name string) {
	if err := s.tracker.Mark(key, source, name); err != nil {
		s.log("  ⚠ Konnte Sync-Status nicht speichern (%v) — Aktivität könnte erneut hochgeladen werden", err)
	}
}

// ---------------------------------------------------------------------------
// MyWhoosh activity sync
// ---------------------------------------------------------------------------

// SyncActivity downloads, fixes and uploads one MyWhoosh activity.
func (s *Syncer) SyncActivity(act mywhoosh.Activity) (ItemResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.ensureMyWhoosh(); err != nil {
		return ItemResult{ID: act.ID, Name: act.DisplayName(), Status: StatusFailed, Message: err.Error()}, err
	}
	return s.processActivity(act, false)
}

// SyncActivityByID looks the activity up in the configured look-back window
// and uploads it.
func (s *Syncer) SyncActivityByID(id string) (ItemResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	activities, err := s.fetchActivities(0)
	if err != nil {
		return ItemResult{ID: id, Status: StatusFailed, Message: err.Error()}, err
	}
	for _, act := range activities {
		if act.ID == id {
			return s.processActivity(act, false)
		}
	}
	err = fmt.Errorf("activity %s not found", id)
	return ItemResult{ID: id, Status: StatusFailed, Message: "Aktivität nicht gefunden"}, err
}

// SyncMyWhoosh fetches outstanding MyWhoosh activities from the last N days
// (0 = configured default), fixes their FIT files and uploads them to Garmin
// Connect. With isAutoSync, activities older than the auto-sync activation
// time are marked as synced instead of uploaded.
func (s *Syncer) SyncMyWhoosh(days int, isAutoSync bool) (Summary, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	summary := Summary{StartedAt: time.Now()}
	finish := func(err error) (Summary, error) {
		summary.FinishedAt = time.Now()
		if err != nil {
			summary.Error = err.Error()
		}
		return summary, err
	}

	activities, err := s.fetchActivities(days)
	if err != nil {
		return finish(err)
	}

	summary.Total = len(activities)
	if summary.Total == 0 {
		s.log("Keine Aktivitäten im gewählten Zeitraum.")
		return finish(nil)
	}
	s.log("✓ %d Aktivitäten gefunden", summary.Total)

	for i, act := range activities {
		s.log("[%d/%d] %s", i+1, summary.Total, act.DisplayName())
		item, err := s.processActivity(act, isAutoSync)
		summary.add(item)
		if errors.Is(err, ErrGarminAuth) {
			s.log("✖ Garmin-Login fehlgeschlagen — Sync wird abgebrochen")
			return finish(err)
		}
	}

	s.log("✓ Sync abgeschlossen — %d hochgeladen, %d Duplikate, %d übersprungen, %d fehlgeschlagen",
		summary.Uploaded, summary.Duplicates, summary.Skipped, summary.Failed)
	return finish(nil)
}

// fetchActivities lists recent activities, logging in again once if the cached
// session was rejected. The caller must hold opMu.
func (s *Syncer) fetchActivities(days int) ([]mywhoosh.Activity, error) {
	cfg := s.Config()
	if days <= 0 {
		days = cfg.EffectiveDays()
	}
	if err := s.ensureMyWhoosh(); err != nil {
		return nil, err
	}

	s.log("MyWhoosh-Aktivitäten (letzte %d Tage) abrufen…", days)
	activities, err := s.mw.GetRecentActivities(days)
	if err != nil && cfg.MyWhooshEmail != "" && cfg.MyWhooshPassword != "" {
		s.log("  Token evtl. abgelaufen, neuer Login…")
		if loginErr := s.mw.Login(cfg.MyWhooshEmail, cfg.MyWhooshPassword); loginErr != nil {
			return nil, fmt.Errorf("MyWhoosh login failed: %w", loginErr)
		}
		activities, err = s.mw.GetRecentActivities(days)
	}
	if err != nil {
		return nil, fmt.Errorf("Aktivitäten abrufen fehlgeschlagen: %w", err)
	}

	s.knownMu.Lock()
	for _, act := range activities {
		s.known[act.ID] = act
	}
	s.knownMu.Unlock()
	return activities, nil
}

// processActivity uploads one activity unless it is already marked as synced.
// The returned error is non-nil exactly when item.Status is StatusFailed. The
// caller must hold opMu.
func (s *Syncer) processActivity(act mywhoosh.Activity, isAutoSync bool) (ItemResult, error) {
	item := ItemResult{ID: act.ID, Name: act.DisplayName()}
	fail := func(msg string, err error) (ItemResult, error) {
		item.Status, item.Message = StatusFailed, msg+err.Error()
		return item, err
	}

	if e, ok := s.tracker.Entry(act.ID); ok {
		item.Status = StatusSkipped
		item.Message = "Bereits synchronisiert"
		if e.Source == SourceManual {
			item.Message = "Manuell als hochgeladen markiert"
		}
		return item, nil
	}

	// Safety net for auto-sync: never upload rides older than the moment it was switched on.
	if cfg := s.Config(); isAutoSync && cfg.AutoSyncActivatedAt > 0 && act.DateUnix() < cfg.AutoSyncActivatedAt {
		s.log("  [Auto-Sync] Überspringe alte Aktivität (vor Aktivierung): %s", act.DisplayName())
		s.mark(act.ID, SourceAutoSkip, act.DisplayName())
		item.Status = StatusSkipped
		item.Message = "Vor Auto-Sync Aktivierung"
		return item, nil
	}

	s.log("▶ %s (%s)", act.DisplayName(), act.FormattedDate())

	fileID := act.ActivityFileID
	if fileID == "" {
		fileID = act.ID
	}
	fitData, err := s.mw.DownloadFitFile(fileID)
	if err != nil {
		return fail("Download fehlgeschlagen: ", err)
	}
	s.log("  ✓ %d Bytes heruntergeladen", len(fitData))

	fixed, err := fitfix.FixFit(fitData, fitfix.LogFunc(s.log))
	if err != nil {
		return fail("FIT-Fix fehlgeschlagen: ", err)
	}

	return s.uploadFixed(&item, act.ID, "mw_"+act.ID+".fit", fixed, act.DisplayName())
}

// uploadFixed sends patched bytes to Garmin and records the outcome under key.
func (s *Syncer) uploadFixed(item *ItemResult, key, fileName string, data []byte, name string) (ItemResult, error) {
	if err := s.ensureGarmin(); err != nil {
		item.Status, item.Message = StatusFailed, "Garmin-Authentifizierung fehlgeschlagen: "+err.Error()
		return *item, fmt.Errorf("%w: %v", ErrGarminAuth, err)
	}

	path, err := writeTempFile(fileName, data)
	if err != nil {
		item.Status, item.Message = StatusFailed, "Temp-Datei erstellen fehlgeschlagen: "+err.Error()
		return *item, err
	}
	defer os.Remove(path)

	s.log("  ⬆ Upload zu Garmin Connect…")
	switch err := s.garmin.Upload(path); {
	case err == nil:
		s.log("  ✓ Hochgeladen")
		item.Status = StatusUploaded
		s.mark(key, SourceUpload, name)
	case errors.Is(err, garmin.ErrDuplicate):
		s.log("  ⚠ Bereits auf Garmin (Duplikat) — als hochgeladen gemerkt")
		item.Status = StatusDuplicate
		s.mark(key, SourceDuplicate, name)
	default:
		item.Status, item.Message = StatusFailed, "Upload fehlgeschlagen: "+err.Error()
		return *item, err
	}
	return *item, nil
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// EnsureGarmin authenticates to Garmin Connect, resuming a cached session,
// refreshing the OAuth2 token, or performing a fresh login when credentials
// are configured. A valid session is cached in dataDir for reuse.
func (s *Syncer) EnsureGarmin() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.ensureGarmin()
}

func (s *Syncer) ensureGarmin() error {
	if s.takeStale(&s.garminStale) {
		s.garmin = nil
		garmin.ClearTokens(s.dataDir)
	}
	if s.garmin != nil && s.garmin.OAuth2 != nil && !s.garmin.OAuth2.Expired() {
		return nil
	}

	client := garmin.NewClient(s.dataDir)
	client.Logf = s.log
	if err := client.Resume(); err == nil {
		s.garmin = client
		return nil
	}

	cfg := s.Config()
	if cfg.GarminEmail == "" || cfg.GarminPassword == "" {
		return fmt.Errorf("Garmin ist nicht eingerichtet — E-Mail & Passwort erforderlich")
	}

	s.log("Garmin Login…")
	if err := client.Login(cfg.GarminEmail, cfg.GarminPassword); err != nil {
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
	if s.takeStale(&s.mwStale) {
		s.mw.ClearToken()
	}
	if s.mw.Resume() == nil {
		return nil
	}

	cfg := s.Config()
	if cfg.MyWhooshEmail == "" || cfg.MyWhooshPassword == "" {
		return fmt.Errorf("MyWhoosh ist nicht eingerichtet — E-Mail & Passwort erforderlich")
	}
	s.log("MyWhoosh Login…")
	if err := s.mw.Login(cfg.MyWhooshEmail, cfg.MyWhooshPassword); err != nil {
		return fmt.Errorf("MyWhoosh login failed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Generic file upload (FIT or TCX)
// ---------------------------------------------------------------------------

// contentHash is the dedup key for uploaded files: SHA-256 of the raw bytes.
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// UploadFileBytes patches and uploads a single activity file. name must carry
// a .fit, .tcx or .xml extension. Files whose content was uploaded before are
// reported as duplicates without contacting Garmin.
func (s *Syncer) UploadFileBytes(name string, data []byte) (ItemResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	item := ItemResult{Name: name}

	var fixed []byte
	var err error
	var ext string
	switch strings.ToLower(filepath.Ext(name)) {
	case ".tcx", ".xml":
		ext = ".tcx"
		if s.tracker.IsSynced(contentHash(data)) {
			item.Status = StatusDuplicate
			return item, nil
		}
		if fixed, err = tcx.FixTcx(data); err != nil {
			return ItemResult{Name: name, Status: StatusFailed, Message: "TCX-Fix: " + err.Error()}, err
		}
	case ".fit":
		ext = ".fit"
		if s.tracker.IsSynced(contentHash(data)) {
			item.Status = StatusDuplicate
			return item, nil
		}
		if fixed, err = fitfix.FixFit(data, fitfix.LogFunc(s.log)); err != nil {
			return ItemResult{Name: name, Status: StatusFailed, Message: "FIT-Fix: " + err.Error()}, err
		}
	default:
		return ItemResult{Name: name, Status: StatusFailed, Message: "Unbekanntes Format (nutze .fit oder .tcx)"},
			fmt.Errorf("unsupported file format: %q", name)
	}

	return s.uploadFixed(&item, contentHash(data), "upload"+ext, fixed, name)
}

// writeTempFile stores bytes in a temp file for the Garmin multipart upload.
// The random suffix is inserted before the file extension so the uploaded
// file keeps a valid .fit/.tcx extension that Garmin can detect.
func writeTempFile(name string, data []byte) (string, error) {
	f, err := os.CreateTemp("", "mywhoosh2garmin-*"+filepath.Ext(name))
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
