package tcx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mywhoosh2garmin/internal/device"
)

func TestFixTcx(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "join_workout.tcx"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(raw) < 1000 {
		t.Fatalf("fixture too small: %d bytes", len(raw))
	}

	fixed, err := FixTcx(raw, device.Identity{})
	if err != nil {
		t.Fatalf("FixTcx failed: %v", err)
	}

	// 1. Author block removed
	if got := findAuthor(fixed); got {
		t.Errorf("JOIN <Author> block was not removed")
	}

	// 2. Garmin Creator block injected before every </Activity>
	wantCreator := `<Creator xsi:type="Device_t"><Name>fenix 6S Pro</Name>`
	if !strings.Contains(string(fixed), wantCreator) {
		t.Errorf("patched TCX missing Garmin Creator block")
	}

	// 3. The Creator must be nested inside the Activity (before </Activity>)
	if !strings.Contains(string(fixed), "</Creator></Activity>") {
		t.Errorf("Creator block is not directly before </Activity>")
	}

	// 4. Track data preserved
	for _, marker := range []string{"<Trackpoint>", "<ns3:Watts>", "<HeartRateBpm>", "<Id>2026-08-31T16:50:17Z</Id>"} {
		if cIn, cOut := strings.Count(string(raw), marker), strings.Count(string(fixed), marker); cIn != cOut {
			t.Errorf("track marker %q differs: in=%d out=%d", marker, cIn, cOut)
		}
	}

	// 5. Total byte size stays roughly the same (only +Creator/-Author)
	if diff := len(fixed) - len(raw); diff > 100 || diff < -100 {
		t.Errorf("unexpected size delta: %d", diff)
	}
}

func TestFixTcxNoAuthorStillWorks(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "join_workout.tcx"))
	if err != nil {
		t.Fatal(err)
	}
	noAuthor := removeAuthor(raw)
	if findAuthor(noAuthor) {
		t.Fatal("removeAuthor did not strip the author block")
	}
	fixed, err := FixTcx(noAuthor, device.Identity{})
	if err != nil {
		t.Fatalf("FixTcx on author-less file: %v", err)
	}
	if !strings.Contains(string(fixed), "<Creator xsi:type=\"Device_t\">") {
		t.Fatal("creator not injected")
	}
}

func TestFixTcxErrors(t *testing.T) {
	if _, err := FixTcx([]byte(""), device.Identity{}); err == nil {
		t.Error("expected error for empty input")
	}
	if _, err := FixTcx([]byte("<foo/>"), device.Identity{}); err == nil {
		t.Error("expected error for non-TCX input")
	}
	if _, err := FixTcx([]byte("<TrainingCenterDatabase xmlns=\"x\"><Activities/></TrainingCenterDatabase>"), device.Identity{}); err == nil {
		t.Error("expected error for TCX without <Activity>")
	}
}

func findAuthor(data []byte) bool {
	return strings.Contains(string(data), "<Author")
}

func TestFixTcxUsesGivenDevice(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "join_workout.tcx"))
	if err != nil {
		t.Fatal(err)
	}
	id := device.Identity{Name: "fenix 8 - 47mm, Solar", Product: 4532, SerialNumber: 3500000001, SoftwareVersion: 2339}
	fixed, err := FixTcx(raw, id)
	if err != nil {
		t.Fatal(err)
	}
	want := `<Name>fenix 8 - 47mm, Solar</Name><UnitId>3500000001</UnitId><ProductID>4532</ProductID><Version><VersionMajor>23</VersionMajor><VersionMinor>39</VersionMinor>`
	if !strings.Contains(string(fixed), want) {
		t.Errorf("creator block does not carry the given device")
	}
}
