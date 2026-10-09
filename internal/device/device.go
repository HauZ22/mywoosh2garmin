// Package device holds the shared Garmin device identity used to spoof
// activity files (MyWhoosh FIT and JOIN TCX) before they are uploaded to
// Garmin Connect. Garmin ignores Training Effect and VO2max for activities
// recorded by unknown manufacturers, so we masquerade as a Garmin fenix 6S.
package device

import "fmt"

// FIT device identifiers (matching typedef.ManufacturerGarmin and
// typedef.GarminProductFenix6s).
const (
	ManufacturerGarmin = 1
	Fenix6SProduct     = 3288
	FakeSerialNumber   = uint32(3420897194)
	Fenix6SName        = "fenix 6S Pro"
)

// Firmware version used in patched TCX files (plausible fenix 6S build).
const (
	VersionMajor = 25
	VersionMinor = 11
	BuildMajor   = 7
	BuildMinor   = 0
)

// CreatorXML returns a TCX <Creator xsi:type="Device_t"> element that mimics
// a Garmin fenix 6S Pro recording device.
func CreatorXML() string {
	return fmt.Sprintf(
		`<Creator xsi:type="Device_t"><Name>%s</Name><UnitId>%d</UnitId><ProductID>%d</ProductID><Version><VersionMajor>%d</VersionMajor><VersionMinor>%d</VersionMinor><BuildMajor>%d</BuildMajor><BuildMinor>%d</BuildMinor></Version></Creator>`,
		Fenix6SName, FakeSerialNumber, Fenix6SProduct,
		VersionMajor, VersionMinor, BuildMajor, BuildMinor,
	)
}
