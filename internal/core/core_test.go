package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mywhoosh2garmin/internal/device"
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

func TestChooseIdentityNeverUsesOwnDevice(t *testing.T) {
	watch := device.Identity{Name: "fenix 8", Product: 4532, SerialNumber: 3500000001, SoftwareVersion: 2339}
	virtual := device.Identity{Name: "Edge 830", Product: 3122, SerialNumber: 3398000000, SoftwareVersion: 975}
	own := map[uint32]string{watch.SerialNumber: "fēnix 8"}

	// Override that is not one of the user's devices is used as-is.
	other := device.Identity{Name: "Edge 1030", Product: 2713, SerialNumber: 3300000123, SoftwareVersion: 1000}
	if id, created, warn := chooseIdentity(other, virtual, own); id != other || created || warn != "" {
		t.Errorf("foreign override: got %+v created=%v warn=%q", id, created, warn)
	}

	// Override with the watch's own unit ID is rejected in favour of the virtual device.
	id, created, warn := chooseIdentity(watch, virtual, own)
	if id != virtual || created || warn == "" {
		t.Errorf("own override: got %+v created=%v warn=%q", id, created, warn)
	}

	// Without a stored device a new virtual one is generated, never the watch.
	id, created, _ = chooseIdentity(device.Identity{}, device.Identity{}, own)
	if !created || id.Product != device.DefaultProduct || id.SerialNumber == watch.SerialNumber {
		t.Errorf("new virtual: got %+v created=%v", id, created)
	}

	// A stored identity that collides with a real device is replaced.
	id, created, _ = chooseIdentity(device.Identity{}, watch, own)
	if !created || id.SerialNumber == watch.SerialNumber {
		t.Errorf("colliding stored: got %+v created=%v", id, created)
	}

	// Device list unknown (nil): stored virtual device is kept.
	if id, created, _ := chooseIdentity(device.Identity{}, virtual, nil); id != virtual || created {
		t.Errorf("unknown devices: got %+v created=%v", id, created)
	}
}
