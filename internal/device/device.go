// Package device holds the Garmin device identity written into activity files
// (MyWhoosh FIT and JOIN TCX) before they are uploaded to Garmin Connect.
//
// Garmin only computes Training Effect, VO2max etc. for activities recorded by
// Garmin devices. The identity must NOT be the user's own watch: Physio TrueUp
// only pushes activities from *other* devices to the watch, so an activity that
// carries the watch's own unit ID is assumed to be on the watch already and
// never reaches it. By default every installation therefore gets its own
// virtual Edge 830 with a random unit ID, generated once and kept in
// virtual_device.json.
package device

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// ManufacturerGarmin is the FIT manufacturer ID of Garmin.
const ManufacturerGarmin = 1

// Identity describes the device written into patched files.
type Identity struct {
	Name            string `json:"name"`
	Product         uint16 `json:"product"`          // FIT garmin_product
	SerialNumber    uint32 `json:"serial_number"`    // Garmin "unit ID"
	SoftwareVersion uint16 `json:"software_version"` // firmware × 100 (23.39 → 2339)
}

// Profile of the virtual device written into patched files by default.
const (
	DefaultName            = "Edge 830"
	DefaultProduct         = 3122 // typedef.GarminProductEdge830
	DefaultSoftwareVersion = 975  // firmware 9.75
)

// Fallback is used only when no valid identity is available at all (e.g. the
// data directory is not writable).
var Fallback = Identity{
	Name:            DefaultName,
	Product:         DefaultProduct,
	SerialNumber:    1,
	SoftwareVersion: DefaultSoftwareVersion,
}

// Range of generated unit IDs (plausible for Garmin devices of that era).
const (
	serialMin = 3_300_000_000
	serialMax = 3_499_999_999
)

// NewVirtual returns a virtual Edge 830 with a random unit ID that is not in
// taken (the unit IDs of the user's real devices; may be nil).
func NewVirtual(taken map[uint32]string) Identity {
	for {
		serial := uint32(serialMin + rand.Uint64N(serialMax-serialMin+1))
		if _, used := taken[serial]; !used {
			return Identity{Name: DefaultName, Product: DefaultProduct, SerialNumber: serial, SoftwareVersion: DefaultSoftwareVersion}
		}
	}
}

// Valid reports whether the identity can be written to a file.
func (id Identity) Valid() bool { return id.Product != 0 && id.SerialNumber != 0 }

// String formats the identity for log output.
func (id Identity) String() string {
	return fmt.Sprintf("Garmin %s (Produkt %d, Unit-ID %d, Firmware %.2f)",
		id.Name, id.Product, id.SerialNumber, float64(id.SoftwareVersion)/100)
}

// CreatorXML returns a TCX <Creator xsi:type="Device_t"> element for id.
func (id Identity) CreatorXML() string {
	return fmt.Sprintf(
		`<Creator xsi:type="Device_t"><Name>%s</Name><UnitId>%d</UnitId><ProductID>%d</ProductID><Version><VersionMajor>%d</VersionMajor><VersionMinor>%d</VersionMinor><BuildMajor>0</BuildMajor><BuildMinor>0</BuildMinor></Version></Creator>`,
		id.Name, id.SerialNumber, id.Product,
		id.SoftwareVersion/100, id.SoftwareVersion%100,
	)
}

const (
	cacheFile  = "virtual_device.json"
	legacyFile = "device.json" // cached copy of the user's own watch (≤ v1.0.1)
)

// RemoveLegacy deletes the device.json written by earlier versions, which held
// the user's own watch and must no longer be used.
func RemoveLegacy(dir string) {
	_ = os.Remove(filepath.Join(dir, legacyFile))
}

// Load reads the stored virtual identity from dir.
func Load(dir string) (Identity, bool) {
	var id Identity
	data, err := os.ReadFile(filepath.Join(dir, cacheFile))
	if err != nil || json.Unmarshal(data, &id) != nil || !id.Valid() {
		return Identity{}, false
	}
	return id, true
}

// Save stores the virtual identity in dir.
func Save(dir string, id Identity) error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cacheFile), data, 0o600)
}
