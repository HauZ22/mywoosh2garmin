package garmin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Device is a watch or bike computer registered in the Garmin Connect account.
type Device struct {
	DeviceID    int64  `json:"deviceId"`
	UnitID      uint32 `json:"unitId"` // numeric serial written to FIT files
	DisplayName string `json:"productDisplayName"`
}

// Devices lists the devices registered in the account.
func (c *Client) Devices() ([]Device, error) {
	var devices []Device
	if err := c.getJSON("/device-service/deviceregistration/devices", &devices); err != nil {
		return nil, err
	}
	return devices, nil
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
