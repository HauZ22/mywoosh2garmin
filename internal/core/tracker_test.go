package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrackerMarkPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st := NewSyncedTracker(dir)
	if st.IsSynced("abc") {
		t.Fatal("new tracker should not consider anything synced")
	}

	if err := st.Mark("abc", SourceManual, "Morning Ride"); err != nil {
		t.Fatal(err)
	}

	reloaded := NewSyncedTracker(dir)
	e, ok := reloaded.Entry("abc")
	if !ok {
		t.Fatal("reloaded tracker must remember 'abc'")
	}
	if e.Source != SourceManual || e.Name != "Morning Ride" || e.At.IsZero() {
		t.Errorf("unexpected entry after reload: %+v", e)
	}
}

func TestTrackerUnmark(t *testing.T) {
	dir := t.TempDir()
	st := NewSyncedTracker(dir)
	_ = st.Mark("a", SourceManual, "")
	_ = st.Mark("b", SourceUpload, "")

	if err := st.Unmark("a"); err != nil {
		t.Fatal(err)
	}
	if err := st.Unmark("never-existed"); err != nil {
		t.Fatalf("unmarking an unknown key must not fail: %v", err)
	}

	reloaded := NewSyncedTracker(dir)
	if reloaded.IsSynced("a") {
		t.Error("'a' should be gone after Unmark + reload")
	}
	if !reloaded.IsSynced("b") {
		t.Error("'b' must stay untouched")
	}
}

func TestTrackerReadsLegacyFormat(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"old-activity": "2026-08-31T18:20:17+02:00", "another": "not-a-date"}`
	if err := os.WriteFile(filepath.Join(dir, "synced.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	st := NewSyncedTracker(dir)
	e, ok := st.Entry("old-activity")
	if !ok || e.Source != SourceUpload {
		t.Fatalf("legacy entry not migrated: %+v ok=%v", e, ok)
	}
	if want := time.Date(2026, 8, 31, 18, 20, 17, 0, time.FixedZone("", 2*3600)); !e.At.Equal(want) {
		t.Errorf("legacy timestamp = %v, want %v", e.At, want)
	}
	if !st.IsSynced("another") {
		t.Error("legacy entries with an unparsable timestamp must still count as synced")
	}

	// A change rewrites the file in the new format and keeps the legacy keys.
	if err := st.Mark("new", SourceManual, "x"); err != nil {
		t.Fatal(err)
	}
	again := NewSyncedTracker(dir)
	for _, k := range []string{"old-activity", "another", "new"} {
		if !again.IsSynced(k) {
			t.Errorf("%q lost after rewrite", k)
		}
	}
}

func TestTrackerCorruptFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "synced.json"), []byte("{not json"), 0o600)
	st := NewSyncedTracker(dir)
	if len(st.Keys()) != 0 {
		t.Error("corrupt file should yield an empty tracker")
	}
	if err := st.Mark("x", SourceUpload, ""); err != nil {
		t.Fatal(err)
	}
}

func TestTrackerLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	st := NewSyncedTracker(dir)
	for _, k := range []string{"a", "b", "c"} {
		if err := st.Mark(k, SourceUpload, ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "synced.json" {
		t.Errorf("expected only synced.json, got %v", entries)
	}
}
