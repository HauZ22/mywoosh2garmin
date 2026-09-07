package fitfix

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
)

// createTestFitFile creates a synthetic MyWhoosh-like FIT file with:
//   - Records that have power, HR, cadence, and temperature set
//   - A session with NO average power, HR, or cadence (simulating the MyWhoosh bug)
func createTestFitFile(t *testing.T, path string) {
	t.Helper()

	now := time.Now()
	activity := filedef.NewActivity()

	activity.FileId.
		SetType(typedef.FileActivity).
		SetTimeCreated(now).
		SetManufacturer(typedef.ManufacturerDevelopment).
		SetProduct(0)

	// Add records with known values: power=200, HR=150, cadence=90, temp=25
	for i := 0; i < 10; i++ {
		rec := mesgdef.NewRecord(nil).
			SetTimestamp(now.Add(time.Duration(i) * time.Second)).
			SetPower(200).
			SetHeartRate(150).
			SetCadence(90).
			SetTemperature(25)
		activity.Records = append(activity.Records, rec)
	}

	// Add a lap
	activity.Laps = append(activity.Laps,
		mesgdef.NewLap(nil).
			SetTimestamp(now.Add(10*time.Second)).
			SetStartTime(now),
	)

	// Add a session with NO averages set (mimics the MyWhoosh bug)
	activity.Sessions = append(activity.Sessions,
		mesgdef.NewSession(nil).
			SetTimestamp(now.Add(10*time.Second)).
			SetStartTime(now).
			SetSport(typedef.SportCycling),
		// AvgPower, AvgHeartRate, AvgCadence are all left at invalid
	)

	activity.Activity = mesgdef.NewActivity(nil).
		SetTimestamp(now.Add(10 * time.Second)).
		SetType(typedef.ActivityManual).
		SetNumSessions(1)

	fit := activity.ToFIT(nil)

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := encoder.New(f).Encode(&fit); err != nil {
		t.Fatal(err)
	}
}

func TestFixFit(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := filepath.Join(tmpDir, "MyNewActivity-3.8.5.fit")

	createTestFitFile(t, inputPath)

	raw, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}

	fixed, err := FixFit(raw)
	if err != nil {
		t.Fatalf("FixFit failed: %v", err)
	}

	// Decode the result and verify.
	f, err := os.OpenFile(filepath.Join(tmpDir, "fixed.fit"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(fixed); err != nil {
		t.Fatal(err)
	}
	f.Close()

	df, err := os.Open(filepath.Join(tmpDir, "fixed.fit"))
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()

	lis := filedef.NewListener()
	defer lis.Close()

	dec := decoder.New(df,
		decoder.WithMesgListener(lis),
		decoder.WithBroadcastOnly(),
	)

	_, err = dec.Decode()
	if err != nil {
		t.Fatalf("decode fixed file: %v", err)
	}

	result, ok := lis.File().(*filedef.Activity)
	if !ok {
		t.Fatalf("expected activity file, got %T", lis.File())
	}

	// Verify temperature was removed from all records
	for i, rec := range result.Records {
		if rec.Temperature != sint8Invalid {
			t.Errorf("record %d: temperature not removed (got %d)", i, rec.Temperature)
		}
	}

	// Verify session averages were set
	if len(result.Sessions) == 0 {
		t.Fatal("no sessions in output")
	}
	sess := result.Sessions[0]

	if sess.AvgPower != 200 {
		t.Errorf("avg power: got %d, want 200", sess.AvgPower)
	}
	if sess.AvgHeartRate != 150 {
		t.Errorf("avg HR: got %d, want 150", sess.AvgHeartRate)
	}
	if sess.AvgCadence != 90 {
		t.Errorf("avg cadence: got %d, want 90", sess.AvgCadence)
	}
}

func TestFindMostRecentFitFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create fake FIT files with different version numbers
	files := []string{
		"MyNewActivity-3.7.0.fit",
		"MyNewActivity-3.8.5.fit",
		"MyNewActivity-3.8.1.fit",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := FindMostRecentFitFile(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(tmpDir, "MyNewActivity-3.8.5.fit")
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestGenerateOutputFilename(t *testing.T) {
	name := GenerateOutputFilename("/some/path/MyNewActivity-3.8.5.fit")
	if !contains(name, "MyNewActivity-3.8.5_") || !contains(name, ".fit") {
		t.Errorf("unexpected filename: %s", name)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstr(s, substr)
}

func searchSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}