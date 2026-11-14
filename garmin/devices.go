package garmin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Device is a watch or bike computer registered in the Garmin Connect account.
type Device struct {
	DeviceID        int64  `json:"deviceId"`
	UnitID          uint32 `json:"unitId"` // numeric serial written to FIT files
	DisplayName     string `json:"productDisplayName"`
	PartNumber      string `json:"partNumber"`             // e.g. "006-B4532-00" → FIT product 4532
	FirmwareVersion string `json:"currentFirmwareVersion"` // e.g. "23.39"
	PrimaryTraining bool   `json:"-"`
}

var partNumberRe = regexp.MustCompile(`^\d{3}-B(\d{4})-\d{2}$`)

// ProductID derives the FIT garmin_product value from the part number
// ("006-B4532-00" → 4532). It returns 0 if the part number has another form.
func (d Device) ProductID() uint16 {
	m := partNumberRe.FindStringSubmatch(strings.TrimSpace(d.PartNumber))
	if m == nil {
		return 0
	}
	n, err := strconv.ParseUint(m[1], 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// SoftwareVersion returns the firmware as FIT software_version (version×100,
// "23.39" → 2339), or 0 if it cannot be parsed.
func (d Device) SoftwareVersion() uint16 {
	f, err := strconv.ParseFloat(strings.TrimSpace(d.FirmwareVersion), 64)
	if err != nil || f <= 0 || f >= 655 {
		return 0
	}
	return uint16(f*100 + 0.5)
}

// Devices lists the devices registered in the account. The device that is
// set as "primary training device" has PrimaryTraining set.
func (c *Client) Devices() ([]Device, error) {
	var devices []Device
	if err := c.getJSON("/device-service/deviceregistration/devices", &devices); err != nil {
		return nil, err
	}

	var primary struct {
		PrimaryTrainingDevice struct {
			DeviceID int64 `json:"deviceId"`
		} `json:"PrimaryTrainingDevice"`
	}
	if err := c.getJSON("/web-gateway/device-info/primary-training-device", &primary); err == nil {
		for i := range devices {
			if devices[i].DeviceID == primary.PrimaryTrainingDevice.DeviceID {
				devices[i].PrimaryTraining = true
			}
		}
	} else {
		c.logf("Primäres Trainingsgerät nicht abrufbar: %v", err)
	}
	return devices, nil
}

// PrimaryTrainingDevice returns the account's primary training device, or —
// if none is marked — the only registered device with a usable FIT product ID.
func (c *Client) PrimaryTrainingDevice() (*Device, error) {
	devices, err := c.Devices()
	if err != nil {
		return nil, err
	}
	var usable []Device
	for _, d := range devices {
		if d.UnitID == 0 || d.ProductID() == 0 {
			continue
		}
		if d.PrimaryTraining {
			return &d, nil
		}
		usable = append(usable, d)
	}
	if len(usable) == 1 {
		return &usable[0], nil
	}
	return nil, fmt.Errorf("kein eindeutiges primäres Trainingsgerät gefunden (%d Geräte)", len(devices))
}

// getJSON performs an authenticated GET against connectapi and decodes the
// JSON response into v. The OAuth2 token is refreshed when needed.
func (c *Client) getJSON(path string, v interface{}) error {
	if c.OAuth2 == nil {
		return fmt.Errorf("not authenticated")
	}
	if c.OAuth2.Expired() {
		if err := c.refreshOAuth2(); err != nil {
			return fmt.Errorf("token refresh: %w", err)
		}
	}

	do := func() (int, []byte, error) {
		req, err := http.NewRequest("GET", fmt.Sprintf("https://connectapi.%s%s", c.Domain, path), nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", c.OAuth2.Bearer())
		req.Header.Set("User-Agent", apiUserAgent)
		req.Header.Set("DI-Backend", fmt.Sprintf("connectapi.%s", c.Domain))
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, body, err
	}

	status, body, err := do()
	if err == nil && status == http.StatusUnauthorized {
		if err := c.refreshOAuth2(); err != nil {
			return fmt.Errorf("token refresh: %w", err)
		}
		status, body, err = do()
	}
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("GET %s: HTTP %d", path, status)
	}
	return json.Unmarshal(body, v)
}
