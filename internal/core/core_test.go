package core

import (
	"path/filepath"
	"testing"

	"mywhoosh2garmin/internal/tcx"
)

func TestConfigEffectiveDays(t *testing.T) {
	cfg := Config{}
	if got := cfg.EffectiveDays(); got != DefaultDays {
		t.Errorf("default days = %d, want %d", got, DefaultDays)
	}
	cfg.Days = 3
	if got := cfg.EffectiveDays(); got != 3 {
		t.Errorf("days = %d, want 3", got)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		MyWhooshEmail:    "mw@example.com",
		MyWhooshPassword: "secret-mw",
		GarminEmail:      "ga@example.com",
		GarminPassword:   "secret-ga",
		Days:             5,
	}
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got := LoadConfig(dir)
	if got != cfg {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	// Missing file → empty config, no error
	if empty := LoadConfig(filepath.Join(t.TempDir(), "nope")); empty != (Config{}) {
		t.Errorf("expected empty config, got %+v", empty)
	}
}

func TestSyncedTracker(t *testing.T) {
	dir := t.TempDir()
	st := NewSyncedTracker(dir)
	if st.IsSynced("abc") {
		t.Fatal("new tracker should not consider anything synced")
	}
	st.MarkSynced("abc")

	st2 := NewSyncedTracker(dir)
	if !st2.IsSynced("abc") {
		t.Fatal("reloaded tracker should remember 'abc'")
	}
}

func TestUploadFileBytesDedupGate(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, Config{})

	// No credentials configured → any real upload attempt would fail at the
	// Garmin login. The dedup gate must short-circuit before that.
	data := []byte("<TrainingCenterDatabase xmlns=\"x\"><Activities><Activity Sport=\"Biking\"><Id>1970-01-01T00:00:00Z</Id></Activity></Activities></TrainingCenterDatabase>")
	key := tcx.ContentHash(data)

	s.Tracker().MarkSynced(key)

	res, err := s.UploadFileBytes("workout.tcx", data)
	if err != nil {
		t.Fatalf("dedup path should not error: %v", err)
	}
	if res.Status != "duplicate" {
		t.Errorf("status = %q, want duplicate", res.Status)
	}
}

func TestUploadFileBytesUnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, Config{})

	res, err := s.UploadFileBytes("route.gpx", []byte("<gpx/>"))
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if res.Status != "failed" {
		t.Errorf("status = %q, want failed", res.Status)
	}
}