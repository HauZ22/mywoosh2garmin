// Package fitfix reads MyWhoosh FIT activity files, fixes missing session
// averages, strips bogus temperature readings and spoofs the recording
// device to a Garmin fenix 6S so Garmin Connect fully processes the file.
package fitfix

import (
	"bytes"
	"fmt"

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

// LogFunc receives format-style log lines (without trailing newline).
type LogFunc func(format string, args ...interface{})

// FixFit reads MyWhoosh FIT activity bytes, fixes missing session averages,
// strips bogus temperature from records, spoofs the device and returns the
// repaired FIT bytes. logf may be nil.
func FixFit(data []byte, logf LogFunc) ([]byte, error) {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}

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

	logf("  Records: %d | Power: %d | HR: %d | Cadence: %d samples",
		len(activity.Records), len(powers), len(heartRates), len(cadences))

	// Fix missing session averages
	for _, sess := range activity.Sessions {
		if shouldFixU16(sess.AvgPower) && len(powers) > 0 {
			sess.AvgPower = avgU16(powers)
			logf("  → Ø Leistung: %d W", sess.AvgPower)
		}
		if shouldFixU8(sess.AvgHeartRate) && len(heartRates) > 0 {
			sess.AvgHeartRate = avgU8(heartRates)
			logf("  → Ø Herzfrequenz: %d bpm", sess.AvgHeartRate)
		}
		if shouldFixU8(sess.AvgCadence) && len(cadences) > 0 {
			sess.AvgCadence = avgU8(cadences)
			logf("  → Ø Trittfrequenz: %d rpm", sess.AvgCadence)
		}
	}

	// Spoof device
	spoofDevice(activity)
	logf("  → Gerät gesetzt: Garmin Fenix 6S Pro (product %d)", device.Fenix6SProduct)

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

func spoofDevice(activity *filedef.Activity) {
	activity.FileId.Manufacturer = typedef.ManufacturerGarmin
	activity.FileId.Product = typedef.GarminProductFenix6s.Uint16()
	activity.FileId.SerialNumber = device.FakeSerialNumber

	for _, di := range activity.DeviceInfos {
		di.Manufacturer = typedef.ManufacturerGarmin
		di.Product = typedef.GarminProductFenix6s.Uint16()
		di.SerialNumber = device.FakeSerialNumber
	}
}
