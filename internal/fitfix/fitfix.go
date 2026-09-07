// Package fitfix reads MyWhoosh FIT activity files, fixes missing session
// averages, strips bogus temperature readings and spoofs the recording
// device to a Garmin fenix 6S so Garmin Connect fully processes the file.
package fitfix

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"

	"mywhoosh2garmin/internal/device"
)

// FIT protocol invalid sentinel values per base type.
const (
	uint8Invalid  = uint8(0xFF)
	uint16Invalid = uint16(0xFFFF)
	sint8Invalid  = int8(0x7F)
)

// logFn can be overridden to redirect log output (e.g. to a GUI or server).
var logFn = func(format string, args ...interface{}) {
	fmt.Printf(format, args...)
}

// SetLog redirects the package log output.
func SetLog(fn func(string, ...interface{})) {
	if fn != nil {
		logFn = fn
	}
}

// ---------------------------------------------------------------------------
// MyWhoosh directory detection
// ---------------------------------------------------------------------------

// IsDir reports whether path is an existing directory.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// FindMyWhooshDir tries to auto-detect the local MyWhoosh data directory.
func FindMyWhooshDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	switch runtime.GOOS {
	case "darwin":
		p := filepath.Join(home,
			"Library", "Containers", "com.whoosh.whooshgame",
			"Data", "Library", "Application Support",
			"Epic", "MyWhoosh", "Content", "Data")
		if IsDir(p) {
			return p, nil
		}
		return "", fmt.Errorf("not found: %s", p)

	case "windows":
		base := filepath.Join(home, "AppData", "Local", "Packages")
		entries, err := os.ReadDir(base)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "MyWhooshTechnologyService.") {
				p := filepath.Join(base, e.Name(),
					"LocalCache", "Local", "MyWhoosh", "Content", "Data")
				if IsDir(p) {
					return p, nil
				}
			}
		}
		return "", fmt.Errorf("MyWhoosh not found in %s", base)

	default:
		return "", fmt.Errorf("no auto-detection for %s — use the directory picker", runtime.GOOS)
	}
}

// ---------------------------------------------------------------------------
// FIT file discovery
// ---------------------------------------------------------------------------

// FindMostRecentFitFile returns the MyNewActivity-*.fit file with the highest
// version number (e.g. MyNewActivity-3.8.5.fit > MyNewActivity-3.7.0.fit).
func FindMostRecentFitFile(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "MyNewActivity-*.fit"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no MyNewActivity-*.fit files in %s", dir)
	}

	re := regexp.MustCompile(`\d+`)
	sort.Slice(matches, func(i, j int) bool {
		vi := re.FindAllString(filepath.Base(matches[i]), -1)
		vj := re.FindAllString(filepath.Base(matches[j]), -1)
		return cmpVersionParts(vi, vj) > 0
	})

	return matches[0], nil
}

func cmpVersionParts(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		ai, _ := strconv.Atoi(a[i])
		bi, _ := strconv.Atoi(b[i])
		if ai != bi {
			return ai - bi
		}
	}
	return len(a) - len(b)
}

// GenerateOutputFilename builds an output filename for a fixed FIT file.
func GenerateOutputFilename(inputPath string) string {
	name := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	ts := time.Now().Format("2006-01-02_150405")
	return fmt.Sprintf("%s_%s.fit", name, ts)
}

// ---------------------------------------------------------------------------
// FIT file processing
// ---------------------------------------------------------------------------

// FixFit reads MyWhoosh FIT activity bytes, fixes missing session averages,
// strips bogus temperature from records, spoofs the device and returns the
// repaired FIT bytes.
func FixFit(data []byte) ([]byte, error) {
	lis := filedef.NewListener()
	defer lis.Close()

	dec := decoder.New(bytes.NewReader(data),
		decoder.WithMesgListener(lis),
		decoder.WithBroadcastOnly(),
	)

	if _, err := dec.Decode(); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	activity, ok := lis.File().(*filedef.Activity)
	if !ok {
		return nil, fmt.Errorf("not an activity file (got %T)", lis.File())
	}

	// Collect metrics from records and strip temperature
	var powers []uint16
	var heartRates, cadences []uint8

	for _, rec := range activity.Records {
		if rec.Power != uint16Invalid {
			powers = append(powers, rec.Power)
		}
		if rec.HeartRate != uint8Invalid {
			heartRates = append(heartRates, rec.HeartRate)
		}
		if rec.Cadence != uint8Invalid {
			cadences = append(cadences, rec.Cadence)
		}
		rec.Temperature = sint8Invalid
	}

	logFn("Records: %d | Power: %d | HR: %d | Cadence: %d samples\n",
		len(activity.Records), len(powers), len(heartRates), len(cadences))

	// Fix missing session averages
	for _, sess := range activity.Sessions {
		if shouldFixU16(sess.AvgPower) && len(powers) > 0 {
			sess.AvgPower = avgU16(powers)
			logFn("  → avg power:      %d W\n", sess.AvgPower)
		}
		if shouldFixU8(sess.AvgHeartRate) && len(heartRates) > 0 {
			sess.AvgHeartRate = avgU8(heartRates)
			logFn("  → avg heart rate: %d bpm\n", sess.AvgHeartRate)
		}
		if shouldFixU8(sess.AvgCadence) && len(cadences) > 0 {
			sess.AvgCadence = avgU8(cadences)
			logFn("  → avg cadence:    %d rpm\n", sess.AvgCadence)
		}
	}

	// Spoof device
	spoofDevice(activity)

	// Encode
	fit := activity.ToFIT(nil)

	var out bytes.Buffer
	if err := encoder.New(&out, encoder.WithProtocolVersion(proto.V2)).Encode(&fit); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), nil
}

func shouldFixU16(v uint16) bool { return v == uint16Invalid || v == 0 }
func shouldFixU8(v uint8) bool   { return v == uint8Invalid || v == 0 }

func avgU16(vals []uint16) uint16 {
	var sum uint64
	for _, v := range vals {
		sum += uint64(v)
	}
	return uint16(sum / uint64(len(vals)))
}

func avgU8(vals []uint8) uint8 {
	var sum uint64
	for _, v := range vals {
		sum += uint64(v)
	}
	return uint8(sum / uint64(len(vals)))
}

// ---------------------------------------------------------------------------
// Device spoofing
// ---------------------------------------------------------------------------

func spoofDevice(activity *filedef.Activity) {
	activity.FileId.Manufacturer = typedef.ManufacturerGarmin
	activity.FileId.Product = typedef.GarminProductFenix6s.Uint16()
	activity.FileId.SerialNumber = device.FakeSerialNumber

	for _, di := range activity.DeviceInfos {
		di.Manufacturer = typedef.ManufacturerGarmin
		di.Product = typedef.GarminProductFenix6s.Uint16()
		di.SerialNumber = device.FakeSerialNumber
	}

	logFn("  → device spoofed: Garmin Fenix 6S Pro (product %d)\n", device.Fenix6SProduct)
}

// ---------------------------------------------------------------------------
// Sync helpers (local-file mode)
// ---------------------------------------------------------------------------

// FindUnsyncedFitFiles returns *.fit files modified in the last 30 days
// that don't have a .synced marker file next to them.
func FindUnsyncedFitFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.fit"))
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().AddDate(0, 0, -30)
	var result []string

	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue
		}
		if IsSynced(path) {
			continue
		}
		result = append(result, path)
	}

	// Sort oldest first so we upload in chronological order
	sort.Slice(result, func(i, j int) bool {
		ii, _ := os.Stat(result[i])
		jj, _ := os.Stat(result[j])
		return ii.ModTime().Before(jj.ModTime())
	})

	return result, nil
}

// IsSynced checks if a .synced marker file exists for the given FIT file.
func IsSynced(fitPath string) bool {
	_, err := os.Stat(fitPath + ".synced")
	return err == nil
}

// MarkSynced creates a .synced marker file next to the FIT file.
func MarkSynced(fitPath string) error {
	return os.WriteFile(fitPath+".synced", []byte(time.Now().Format(time.RFC3339)), 0o644)
}