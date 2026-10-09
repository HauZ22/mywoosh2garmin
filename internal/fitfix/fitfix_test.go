package fitfix

import (
	"bytes"
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

	fixed, err := FixFit(raw, nil)
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

func TestFixFitSpoofsDevice(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "in.fit")
	createTestFitFile(t, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var logged int
	fixed, err := FixFit(raw, func(string, ...interface{}) { logged++ })
	if err != nil {
		t.Fatal(err)
	}
	if logged == 0 {
		t.Error("expected log output")
	}

	lis := filedef.NewListener()
	defer lis.Close()
	dec := decoder.New(bytes.NewReader(fixed), decoder.WithMesgListener(lis), decoder.WithBroadcastOnly())
	if _, err := dec.Decode(); err != nil {
		t.Fatal(err)
	}
	act := lis.File().(*filedef.Activity)
	if act.FileId.Manufacturer != typedef.ManufacturerGarmin {
		t.Errorf("manufacturer = %v, want Garmin", act.FileId.Manufacturer)
	}
	if act.FileId.Product != typedef.GarminProductFenix6s.Uint16() {
		t.Errorf("product = %d, want fenix 6s", act.FileId.Product)
	}
}

func TestFixFitRejectsGarbage(t *testing.T) {
	if _, err := FixFit([]byte("this is not a FIT file"), nil); err == nil {
		t.Fatal("expected decode error")
	}
}
