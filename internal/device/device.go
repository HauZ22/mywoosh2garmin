// Package device holds the Garmin device identity written into activity files
// (MyWhoosh FIT and JOIN TCX) before they are uploaded to Garmin Connect.
//
// Garmin only computes Training Effect, VO2max etc. for activities recorded by
// Garmin devices, and physiological metrics are stored per device. The files
// should therefore carry the user's own primary training device (product ID +
// unit ID from the Garmin account), so the results are credited to that watch
// and reach it via Physio TrueUp. Fallback is a generic fenix 6S identity.
package device

import (
	"encoding/json"
	"fmt"
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

// Fallback is used when the user's own device is unknown.
var Fallback = Identity{
	Name:            "fenix 6S Pro",
	Product:         3288, // typedef.GarminProductFenix6s
	SerialNumber:    3420897194,
	SoftwareVersion: 2511,
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

const cacheFile = "device.json"

// Load reads the cached identity from dir.
func Load(dir string) (Identity, bool) {
	var id Identity
	data, err := os.ReadFile(filepath.Join(dir, cacheFile))
	if err != nil || json.Unmarshal(data, &id) != nil || !id.Valid() {
		return Identity{}, false
	}
	return id, true
}

// Save caches the identity in dir.
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
