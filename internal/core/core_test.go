package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mywhoosh2garmin/mywhoosh"
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

func TestUploadFileBytesDedupGate(t *testing.T) {
	s := New(t.TempDir(), Config{})

	// No credentials configured → any real upload attempt would fail at the
	// Garmin login. The dedup gate must short-circuit before that.
	data := []byte("<TrainingCenterDatabase xmlns=\"x\"><Activities><Activity Sport=\"Biking\"><Id>1970-01-01T00:00:00Z</Id></Activity></Activities></TrainingCenterDatabase>")
	if err := s.Tracker().Mark(contentHash(data), SourceUpload, ""); err != nil {
		t.Fatal(err)
	}

	res, err := s.UploadFileBytes("workout.tcx", data)
	if err != nil {
		t.Fatalf("dedup path should not error: %v", err)
	}
	if res.Status != StatusDuplicate {
		t.Errorf("status = %q, want duplicate", res.Status)
	}
}

func TestUploadFileBytesUnsupportedFormat(t *testing.T) {
	s := New(t.TempDir(), Config{})

	res, err := s.UploadFileBytes("route.gpx", []byte("<gpx/>"))
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if res.Status != StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
}

func TestUploadFileBytesWithoutGarminAccountFails(t *testing.T) {
	s := New(t.TempDir(), Config{})
	data := []byte("<TrainingCenterDatabase><Activities><Activity Sport=\"Biking\"></Activity></Activities></TrainingCenterDatabase>")

	res, err := s.UploadFileBytes("w.tcx", data)
	if err == nil || res.Status != StatusFailed {
		t.Fatalf("want failed + error, got %+v / %v", res, err)
	}
	if !strings.Contains(res.Message, "Garmin") {
		t.Errorf("message should point at Garmin setup: %q", res.Message)
	}
}

func TestSummaryCountsEveryStatus(t *testing.T) {
	var sum Summary
	for _, st := range []string{StatusUploaded, StatusUploaded, StatusDuplicate, StatusSkipped, StatusFailed} {
		sum.add(ItemResult{Status: st})
	}
	if sum.Uploaded != 2 || sum.Duplicates != 1 || sum.Skipped != 1 || sum.Failed != 1 || len(sum.Items) != 5 {
		t.Errorf("unexpected tally: %+v", sum)
	}
}

func TestManualMarkSkipsActivityPermanently(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, Config{})
	s.known["42"] = mywhoosh.Activity{ID: "42", Name: "Sunday Ride"}

	n, err := s.SetActivitiesSynced([]string{"42", ""}, true)
	if err != nil || n != 1 {
		t.Fatalf("SetActivitiesSynced = %d, %v; want 1, nil", n, err)
	}
	// Marking twice changes nothing.
	if n, _ := s.SetActivitiesSynced([]string{"42"}, true); n != 0 {
		t.Errorf("second mark changed %d entries, want 0", n)
	}

	// A fresh process (new Syncer on the same dir) still skips it – without any network access.
	restarted := New(dir, Config{})
	item, err := restarted.processActivity(mywhoosh.Activity{ID: "42", Name: "Sunday Ride"}, false)
	if err != nil {
		t.Fatalf("skip must not fail: %v", err)
	}
	if item.Status != StatusSkipped || !strings.Contains(item.Message, "Manuell") {
		t.Errorf("item = %+v, want skipped/manual", item)
	}
	e, _ := restarted.Tracker().Entry("42")
	if e.Source != SourceManual || e.Name != "Sunday Ride" {
		t.Errorf("entry = %+v", e)
	}

	// Undo makes it eligible again.
	if n, _ := restarted.SetActivitiesSynced([]string{"42"}, false); n != 1 {
		t.Errorf("unmark changed %d, want 1", n)
	}
	if restarted.Tracker().IsSynced("42") {
		t.Error("still synced after unmark")
	}
}

func TestAutoSyncSkipsActivitiesOlderThanActivation(t *testing.T) {
	s := New(t.TempDir(), Config{AutoSyncActivatedAt: time.Now().Unix()})
	old := mywhoosh.Activity{ID: "old", Name: "Old ride", Date: float64(time.Now().Add(-48 * time.Hour).Unix())}

	item, err := s.processActivity(old, true)
	if err != nil || item.Status != StatusSkipped {
		t.Fatalf("got %+v / %v, want skipped", item, err)
	}
	if e, ok := s.Tracker().Entry("old"); !ok || e.Source != SourceAutoSkip {
		t.Errorf("old ride must be remembered as auto-skip, got %+v ok=%v", e, ok)
	}
}

func TestApplyConfigFlagsAccountChange(t *testing.T) {
	s := New(t.TempDir(), Config{MyWhooshEmail: "a@x.io", GarminEmail: "g@x.io"})

	s.ApplyConfig(Config{MyWhooshEmail: "a@x.io", GarminEmail: "g@x.io", Days: 3})
	if s.mwStale || s.garminStale {
		t.Error("unchanged accounts must keep their sessions")
	}

	s.ApplyConfig(Config{MyWhooshEmail: "other@x.io", GarminEmail: "g@x.io"})
	if !s.mwStale || s.garminStale {
		t.Errorf("only the MyWhoosh session should be stale: mw=%v garmin=%v", s.mwStale, s.garminStale)
	}
}
