package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SyncSource records why an activity counts as "already on Garmin".
type SyncSource string

const (
	// SourceUpload: this tool uploaded the activity successfully.
	SourceUpload SyncSource = "upload"
	// SourceDuplicate: Garmin rejected the upload because it already had the activity.
	SourceDuplicate SyncSource = "duplicate"
	// SourceManual: the user marked the activity as uploaded by hand.
	SourceManual SyncSource = "manual"
	// SourceAutoSkip: older than the moment auto-sync was switched on.
	SourceAutoSkip SyncSource = "auto-skip"
)

// SyncEntry is one remembered activity.
type SyncEntry struct {
	At     time.Time  `json:"at"`
	Source SyncSource `json:"source"`
	Name   string     `json:"name,omitempty"`
}

// SyncedTracker persists the set of activities that must not be uploaded
// again (stored in synced.json, so it survives restarts and updates). Entries
// are only ever removed explicitly via Unmark.
type SyncedTracker struct {
	mu      sync.Mutex
	path    string
	entries map[string]SyncEntry
}

// NewSyncedTracker loads (or initialises) the tracker stored in dir/synced.json.
// Both the current format and the legacy {"key": "<RFC3339 timestamp>"} format
// are understood; the file is rewritten in the current format on the next change.
func NewSyncedTracker(dir string) *SyncedTracker {
	st := &SyncedTracker{
		path:    filepath.Join(dir, "synced.json"),
		entries: make(map[string]SyncEntry),
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return st
	}

	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return st
	}
	for key, v := range raw {
		var entry SyncEntry
		if json.Unmarshal(v, &entry) == nil && entry.Source != "" {
			st.entries[key] = entry
			continue
		}
		// Legacy format: value is just the upload timestamp.
		var ts string
		if json.Unmarshal(v, &ts) == nil {
			at, _ := time.Parse(time.RFC3339, ts)
			st.entries[key] = SyncEntry{At: at, Source: SourceUpload}
		}
	}
	return st
}

// IsSynced reports whether the given activity key is marked as uploaded.
func (st *SyncedTracker) IsSynced(key string) bool {
	_, ok := st.Entry(key)
	return ok
}

// Entry returns the stored entry for key.
func (st *SyncedTracker) Entry(key string) (SyncEntry, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.entries[key]
	return e, ok
}

// Mark records key as uploaded and persists the change. The in-memory state is
// updated even if writing fails, so the error is only about durability.
func (st *SyncedTracker) Mark(key string, source SyncSource, name string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.entries[key] = SyncEntry{At: time.Now(), Source: source, Name: name}
	return st.saveLocked()
}

// Unmark removes the marker again (e.g. after a mis-click) so the activity
// becomes eligible for upload.
func (st *SyncedTracker) Unmark(key string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.entries[key]; !ok {
		return nil
	}
	delete(st.entries, key)
	return st.saveLocked()
}

// Keys returns all remembered keys, sorted.
func (st *SyncedTracker) Keys() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	keys := make([]string, 0, len(st.entries))
	for k := range st.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// saveLocked writes the file atomically (temp file + rename) so a crash while
// saving can never truncate the history. Callers must hold st.mu.
func (st *SyncedTracker) saveLocked() error {
	data, err := json.MarshalIndent(st.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), "synced-*.tmp")
	if err != nil {
		return fmt.Errorf("synced.json: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("synced.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("synced.json: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("synced.json: %w", err)
	}
	if err := os.Rename(tmp.Name(), st.path); err != nil {
		return fmt.Errorf("synced.json: %w", err)
	}
	return nil
}
