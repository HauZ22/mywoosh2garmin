// Package tcx reads JOIN Cycling workout player TCX files and rewrites them
// so Garmin Connect fully processes the activity: the 3rd-party <Author>
// element is removed and a Garmin fenix 6S device <Creator> block is injected
// into every activity. Track data is left byte-for-byte intact.
package tcx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"mywhoosh2garmin/internal/device"
)

const closingActivity = "</Activity>"

// ContentHash returns a stable SHA-256 hex digest of the raw file bytes.
// It is used as the dedup key for TCX (and generic file) uploads.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// FixTcx patches a JOIN Cycling TCX file:
//
//  1. removes the <Author ...>...</Author> application block, and
//  2. inserts a Garmin device <Creator> block before every </Activity> tag.
//
// The track points, laps and all timing/power/HR/cadence data are preserved.
func FixTcx(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("empty TCX file")
	}
	if !bytes.Contains(data, []byte("<TrainingCenterDatabase")) {
		return nil, errors.New("not a TrainingCenterDatabase (TCX) file")
	}

	out := removeAuthor(data)
	creatorXML := []byte(device.CreatorXML())

	var b bytes.Buffer
	b.Grow(len(out) + len(creatorXML))
	found := false
	rest := out
	for {
		idx := bytes.Index(rest, []byte(closingActivity))
		if idx < 0 {
			if !found {
				return nil, errors.New("no <Activity> element found in TCX")
			}
			b.Write(rest)
			break
		}
		found = true
		b.Write(rest[:idx])
		b.Write(creatorXML)
		b.Write(rest[idx : idx+len(closingActivity)])
		rest = rest[idx+len(closingActivity):]
	}
	return b.Bytes(), nil
}

// removeAuthor strips the trailing <Author ...>...</Author> block (if present).
func removeAuthor(data []byte) []byte {
	start := bytes.Index(data, []byte("<Author"))
	if start < 0 {
		return data
	}
	endRel := bytes.Index(data[start:], []byte("</Author>"))
	if endRel < 0 {
		return data
	}
	end := start + endRel + len("</Author>")
	out := make([]byte, 0, len(data)-(end-start))
	out = append(out, data[:start]...)
	out = append(out, data[end:]...)
	return out
}