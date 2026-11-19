// Package fitfix reads MyWhoosh FIT activity files and repairs them so Garmin
// Connect processes them like a recording from a Garmin watch: missing session
// averages are filled in, bogus temperatures removed, broken timestamps and
// timer events corrected, and the user's Garmin device is written as creator.
package fitfix

import (
	"bytes"
	"fmt"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"

	"mywhoosh2garmin/internal/device"
)

// FIT protocol invalid sentinel values per base type.
const (
	uint8Invalid  = uint8(0xFF)
	uint16Invalid = uint16(0xFFFF)
	uint32Invalid = uint32(0xFFFFFFFF)
	sint8Invalid  = int8(0x7F)
)

// LogFunc receives format-style log lines (without trailing newline).
type LogFunc func(format string, args ...interface{})

// FixFit reads MyWhoosh FIT activity bytes, repairs them and writes id as the
// recording device (device.Fallback if id is not valid). Local times are
// derived from loc (nil = time.Local). logf may be nil.
func FixFit(data []byte, id device.Identity, loc *time.Location, logf LogFunc) ([]byte, error) {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	if !id.Valid() {
		id = device.Fallback
	}
	if loc == nil {
		loc = time.Local
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
	var maxSpeed uint32 // mm/s

	for _, rec := range activity.Records {
		if s := recordSpeed(rec); s > maxSpeed {
			maxSpeed = s
		}
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
	if len(heartRates) == 0 {
		logf("  ⚠ Keine Herzfrequenz in der Datei — Garmin kann so keine VO2max / Training Effect berechnen")
	}
	if len(powers) == 0 {
		logf("  ⚠ Keine Leistungsdaten in der Datei — ohne Leistung keine Rad-VO2max")
	}

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
		if fixSessionSpeed(sess, maxSpeed) {
			logf("  → Ø Geschwindigkeit: %.1f km/h", float64(sess.EnhancedAvgSpeed)*3.6/1000)
		}
	}

	if fixTimes(activity, loc) {
		logf("  → Zeitstempel / lokale Zeit korrigiert")
	}
	if fixTimerEvents(activity) {
		logf("  → Timer-Events ergänzt")
	}

	setDevice(activity, id)
	logf("  → Gerät gesetzt: %s", id)

	// Encode
	fit := activity.ToFIT(nil)

	var out bytes.Buffer
	if err := encoder.New(&out, encoder.WithProtocolVersion(proto.V2)).Encode(&fit); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), nil
}

// recordSpeed returns the record's speed in mm/s (0 if unset).
func recordSpeed(r *mesgdef.Record) uint32 {
	if r.EnhancedSpeed != uint32Invalid && r.EnhancedSpeed != 0 {
		return r.EnhancedSpeed
	}
	if r.Speed != uint16Invalid {
		return uint32(r.Speed)
	}
	return 0
}

// fixSessionSpeed fills in average and maximum speed. MyWhoosh writes 0 as
// average speed although total distance and timer time are known. maxSpeed is
// the highest record speed in mm/s. Reports whether the average was set.
func fixSessionSpeed(s *mesgdef.Session, maxSpeed uint32) bool {
	if shouldFixU16(s.MaxSpeed) && (s.EnhancedMaxSpeed == uint32Invalid || s.EnhancedMaxSpeed == 0) && maxSpeed > 0 {
		s.EnhancedMaxSpeed = maxSpeed
		if maxSpeed < uint32(uint16Invalid) {
			s.MaxSpeed = uint16(maxSpeed)
		}
	}
	if !shouldFixU16(s.AvgSpeed) || (s.EnhancedAvgSpeed != uint32Invalid && s.EnhancedAvgSpeed != 0) {
		return false
	}
	if s.TotalDistance == uint32Invalid || s.TotalDistance == 0 ||
		s.TotalTimerTime == uint32Invalid || s.TotalTimerTime == 0 {
		return false
	}
	// distance in cm, time in ms → speed in mm/s
	avg := uint64(s.TotalDistance) * 10000 / uint64(s.TotalTimerTime)
	if avg >= uint64(uint32Invalid) {
		return false
	}
	s.EnhancedAvgSpeed = uint32(avg)
	if avg < uint64(uint16Invalid) {
		s.AvgSpeed = uint16(avg)
	}
	return true
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

func elapsed(ms uint32) time.Duration { return time.Duration(ms) * time.Millisecond }

// activityBounds returns the start and end time of the recording.
func activityBounds(a *filedef.Activity) (start, end time.Time) {
	for _, r := range a.Records {
		if r.Timestamp.IsZero() {
			continue
		}
		if start.IsZero() || r.Timestamp.Before(start) {
			start = r.Timestamp
		}
		if r.Timestamp.After(end) {
			end = r.Timestamp
		}
	}
	for _, s := range a.Sessions {
		if s.StartTime.IsZero() {
			continue
		}
		if start.IsZero() || s.StartTime.Before(start) {
			start = s.StartTime
		}
		if s.TotalElapsedTime != uint32Invalid {
			if e := s.StartTime.Add(elapsed(s.TotalElapsedTime)); e.After(end) {
				end = e
			}
		}
	}
	return start, end
}

// fixTimes repairs summary timestamps. MyWhoosh writes the start time as
// session/activity timestamp (FIT expects the end time) and a local_timestamp
// in the wrong epoch, which breaks the time-zone offset Garmin derives from
// it. Reports whether anything was changed.
func fixTimes(a *filedef.Activity, loc *time.Location) bool {
	_, end := activityBounds(a)
	if end.IsZero() {
		return false
	}
	changed := false
	for _, s := range a.Sessions {
		if !s.Timestamp.IsZero() && s.Timestamp.After(s.StartTime) {
			continue
		}
		if !s.StartTime.IsZero() && s.TotalElapsedTime != uint32Invalid {
			s.Timestamp = s.StartTime.Add(elapsed(s.TotalElapsedTime))
		} else {
			s.Timestamp = end
		}
		changed = true
	}
	for _, l := range a.Laps {
		if (l.Timestamp.IsZero() || !l.Timestamp.After(l.StartTime)) &&
			!l.StartTime.IsZero() && l.TotalElapsedTime != uint32Invalid {
			l.Timestamp = l.StartTime.Add(elapsed(l.TotalElapsedTime))
			changed = true
		}
	}
	if a.Activity == nil {
		return changed
	}
	if a.Activity.Timestamp.IsZero() || a.Activity.Timestamp.Before(end) {
		a.Activity.Timestamp = end
		changed = true
	}
	_, offset := a.Activity.Timestamp.In(loc).Zone()
	local := a.Activity.Timestamp.Add(time.Duration(offset) * time.Second)
	if !a.Activity.LocalTimestamp.Equal(local) {
		a.Activity.LocalTimestamp = local
		changed = true
	}
	return changed
}

// fixTimerEvents makes sure the recording starts with a timer/start event and
// ends with a timer/stop_all event, as Garmin devices write them. MyWhoosh's
// first event has neither event nor event_type set.
func fixTimerEvents(a *filedef.Activity) bool {
	start, end := activityBounds(a)
	if start.IsZero() {
		return false
	}
	changed := false
	hasStart, hasStop := false, false
	for i, e := range a.Events {
		if i == 0 && e.Event == typedef.EventInvalid && e.EventType == typedef.EventTypeInvalid {
			e.Event, e.EventType, e.EventGroup = typedef.EventTimer, typedef.EventTypeStart, 0
			changed = true
		}
		if e.Event != typedef.EventTimer {
			continue
		}
		switch e.EventType {
		case typedef.EventTypeStart:
			hasStart = true
		case typedef.EventTypeStop, typedef.EventTypeStopAll:
			hasStop = true
		}
	}
	if !hasStart {
		ev := mesgdef.NewEvent(nil)
		ev.Timestamp, ev.Event, ev.EventType, ev.EventGroup = start, typedef.EventTimer, typedef.EventTypeStart, 0
		a.Events = append([]*mesgdef.Event{ev}, a.Events...)
		changed = true
	}
	if !hasStop {
		ev := mesgdef.NewEvent(nil)
		ev.Timestamp, ev.Event, ev.EventType, ev.EventGroup = end, typedef.EventTimer, typedef.EventTypeStopAll, 0
		// Insert before a closing session event, otherwise append.
		idx := len(a.Events)
		for i, e := range a.Events {
			if e.Event == typedef.EventSession && !e.Timestamp.Before(end) {
				idx = i
				break
			}
		}
		a.Events = append(a.Events[:idx], append([]*mesgdef.Event{ev}, a.Events[idx:]...)...)
		changed = true
	}
	return changed
}

// setDevice writes id as the recording device: file_id, file_creator and a
// device_info message for the creator (device index 0). Device infos of
// sensors (power meter, HR strap, …) are left untouched.
func setDevice(a *filedef.Activity, id device.Identity) {
	a.FileId.Manufacturer = typedef.ManufacturerGarmin
	a.FileId.Product = id.Product
	a.FileId.SerialNumber = id.SerialNumber

	start, _ := activityBounds(a)
	if start.IsZero() {
		start = a.FileId.TimeCreated
	}

	var creator *mesgdef.DeviceInfo
	for _, di := range a.DeviceInfos {
		if di.DeviceIndex == typedef.DeviceIndexCreator {
			creator = di
			break
		}
	}
	if creator == nil {
		creator = mesgdef.NewDeviceInfo(nil)
		creator.Timestamp = start
		creator.DeviceIndex = typedef.DeviceIndexCreator
		a.DeviceInfos = append([]*mesgdef.DeviceInfo{creator}, a.DeviceInfos...)
	}
	creator.Manufacturer = typedef.ManufacturerGarmin
	creator.Product = id.Product
	creator.SerialNumber = id.SerialNumber
	creator.SoftwareVersion = id.SoftwareVersion
	creator.SourceType = typedef.SourceTypeLocal
	creator.ProductName = ""

	// file_creator is not part of filedef.Activity; patch it in place.
	for i := range a.UnrelatedMessages {
		m := &a.UnrelatedMessages[i]
		if m.Num != typedef.MesgNumFileCreator {
			continue
		}
		for j := range m.Fields {
			if m.Fields[j].Num == 0 { // software_version
				m.Fields[j].Value = proto.Uint16(id.SoftwareVersion)
			}
		}
	}
}
