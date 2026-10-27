// Package garmin implements the Garmin Connect SSO login (OAuth1 → OAuth2)
// and the activity-file upload.
package garmin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const apiUserAgent = "GCM-iOS-5.19.1.2"
const uploadProcessingTimeout = 2 * time.Minute

// ErrDuplicate is returned by Upload when Garmin Connect already has the activity.
var ErrDuplicate = errors.New("duplicate activity (already uploaded to Garmin)")

// Client manages authentication and uploads to Garmin Connect.
type Client struct {
	OAuth1   *OAuth1Token
	OAuth2   *OAuth2Token
	Domain   string
	TokenDir string // directory where tokens are cached

	// PromptMFA supplies the two-factor code during Login; nil means accounts
	// with 2FA cannot log in.
	PromptMFA MFAPrompt

	apiBaseURL   string        // injectable in tests; defaults to connectapi.<Domain>
	httpClient   *http.Client  // injectable in tests
	pollInterval time.Duration // injectable in tests

	// Logf receives progress messages; nil silences them.
	Logf func(format string, args ...interface{})
}

// NewClient creates a Client that caches tokens in tokenDir.
func NewClient(tokenDir string) *Client {
	return &Client{
		Domain:   "garmin.com",
		TokenDir: tokenDir,
	}
}

func (c *Client) logf(format string, args ...interface{}) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Resume tries to load cached tokens and refresh if needed.
// Returns nil if a valid session was restored, error otherwise.
func (c *Client) Resume() error {
	if c.TokenDir == "" {
		return fmt.Errorf("no token directory configured")
	}

	oauth1, oauth2, err := LoadTokens(c.TokenDir)
	if err != nil {
		return fmt.Errorf("no cached session: %w", err)
	}

	c.OAuth1 = oauth1
	c.OAuth2 = oauth2
	if oauth1.Domain != "" {
		c.Domain = oauth1.Domain
	}

	// Token still valid
	if !oauth2.Expired() {
		return nil
	}

	// OAuth2 expired — try to refresh using OAuth1 (lasts ~1 year)
	c.logf("Garmin-Session abgelaufen, wird erneuert…")
	if err := c.refreshOAuth2(); err != nil {
		return fmt.Errorf("refresh failed: %w", err)
	}
	return nil
}

// Login performs a fresh SSO login with the given credentials.
func (c *Client) Login(email, password string) error {
	oauth1, oauth2, err := Login(email, password, c.Domain, c.PromptMFA)
	if err != nil {
		return err
	}

	c.OAuth1 = oauth1
	c.OAuth2 = oauth2

	// Cache tokens for next time
	if c.TokenDir != "" {
		if err := SaveTokens(c.TokenDir, oauth1, oauth2); err != nil {
			c.logf("Warnung: Tokens konnten nicht gespeichert werden: %v", err)
		}
	}
	return nil
}

// Upload sends a FIT or TCX activity file to Garmin Connect (the format is
// selected by the file extension). It waits for Garmin's asynchronous import
// to finish before returning success, refreshes OAuth2 once on 401, and returns
// ErrDuplicate if Garmin already knows the activity.
func (c *Client) Upload(filePath string) error {
	if c.OAuth2 == nil {
		return fmt.Errorf("not authenticated")
	}

	// Auto-refresh if expired
	if c.OAuth2.Expired() {
		if err := c.refreshOAuth2(); err != nil {
			return fmt.Errorf("token refresh: %w", err)
		}
	}

	// First attempt
	status, headers, body, err := c.doUpload(filePath)
	if err != nil {
		return err
	}

	// Retry once on 401 (token might be stale despite not being expired)
	if status == http.StatusUnauthorized {
		c.logf("Garmin-Token abgelehnt, wird erneuert…")
		if err := c.refreshOAuth2(); err != nil {
			return fmt.Errorf("token refresh: %w", err)
		}
		status, headers, body, err = c.doUpload(filePath)
		if err != nil {
			return err
		}
	}

	return c.finishUpload(status, headers, body)
}

// refreshOAuth2 exchanges the OAuth1 token for a fresh OAuth2 token.
func (c *Client) refreshOAuth2() error {
	oauth2, err := ExchangeForOAuth2(c.OAuth1)
	if err != nil {
		return err
	}
	c.OAuth2 = oauth2

	// Update cache (only OAuth2, keep OAuth1 as-is)
	if c.TokenDir != "" {
		_ = SaveTokens(c.TokenDir, nil, oauth2)
	}
	return nil
}

// doUpload performs the actual multipart upload and returns status + body.
func (c *Client) doUpload(filePath string) (int, http.Header, []byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, nil, nil, err
	}
	defer f.Close()

	// Build multipart request
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return 0, nil, nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return 0, nil, nil, err
	}
	if err := writer.Close(); err != nil {
		return 0, nil, nil, err
	}

	uploadURL := c.apiURL("/upload-service/upload/" + strings.TrimPrefix(strings.ToLower(filepath.Ext(filePath)), "."))
	req, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", c.OAuth2.Bearer())
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", apiUserAgent)
	req.Header.Set("DI-Backend", fmt.Sprintf("connectapi.%s", c.Domain))
	req.Header.Set("NK", "NT")

	resp, err := c.client().Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("upload request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}

	return resp.StatusCode, resp.Header.Clone(), body, nil
}

func (c *Client) apiURL(path string) string {
	base := c.apiBaseURL
	if base == "" {
		base = "https://connectapi." + c.Domain
	}
	return strings.TrimRight(base, "/") + path
}

func (c *Client) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) finishUpload(status int, headers http.Header, body []byte) error {
	switch status {
	case http.StatusAccepted:
		location := headers.Get("Location")
		if location == "" {
			return fmt.Errorf("Garmin accepted the upload (HTTP 202) but returned no processing Location; import completion is unconfirmed")
		}
		c.logf("Garmin hat den Upload angenommen (HTTP 202); warte auf Importbestätigung…")
		return c.waitForUpload(location)
	case http.StatusCreated:
		if location := headers.Get("Location"); location != "" {
			if _, err := c.resolveLocation(location); err != nil {
				return err
			}
			if len(body) > 0 {
				if err := parseImportBody(body); err != nil {
					return err
				}
			}
			c.logf("Garmin-Import bestätigt (HTTP 201, Aktivität: %s)", locationID(location))
			return nil
		}
		return parseUploadResult(status, body)
	default:
		return parseUploadResult(status, body)
	}
}

func (c *Client) waitForUpload(location string) error {
	pollURL, err := c.resolveLocation(location)
	if err != nil {
		return err
	}
	interval := c.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(uploadProcessingTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		req, err := http.NewRequest(http.MethodGet, pollURL.String(), nil)
		if err != nil {
			return fmt.Errorf("create Garmin import status request: %w", err)
		}
		req.Header.Set("Authorization", c.OAuth2.Bearer())
		req.Header.Set("User-Agent", apiUserAgent)
		req.Header.Set("DI-Backend", "connectapi."+c.Domain)
		req.Header.Set("NK", "NT")
		resp, err := c.client().Do(req)
		if err != nil {
			return fmt.Errorf("check Garmin import status: %w", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read Garmin import status: %w", readErr)
		}
		switch resp.StatusCode {
		case http.StatusAccepted:
			if next := resp.Header.Get("Location"); next != "" {
				nextURL, parseErr := c.resolveLocation(next)
				if parseErr != nil {
					return fmt.Errorf("Garmin returned an invalid upload status Location: %w", parseErr)
				}
				pollURL = nextURL
			}
			continue
		case http.StatusCreated:
			activityLocation := resp.Header.Get("Location")
			if activityLocation != "" {
				if _, err := c.resolveLocation(activityLocation); err != nil {
					return err
				}
			} else if err := parseImportBody(body); err != nil {
				return fmt.Errorf("Garmin returned HTTP 201 without an activity Location: %w", err)
			}
			if activityLocation == "" {
				activityLocation = pollURL.String()
			}
			c.logf("Garmin-Import bestätigt (HTTP 201, Aktivität: %s)", locationID(activityLocation))
			return nil
		default:
			return parseUploadResult(resp.StatusCode, body)
		}
	}
	return fmt.Errorf("Garmin upload was accepted but import was not confirmed within %s", uploadProcessingTimeout)
}

func (c *Client) resolveLocation(location string) (*url.URL, error) {
	base, err := url.Parse(c.apiURL("/"))
	if err != nil {
		return nil, fmt.Errorf("invalid Garmin API base URL: %w", err)
	}
	resolved, err := base.Parse(location)
	if err != nil || resolved.Host != base.Host || resolved.Scheme != base.Scheme {
		return nil, fmt.Errorf("Garmin returned an invalid or cross-host upload Location; import completion is unconfirmed")
	}
	return resolved, nil
}

func locationID(location string) string {
	parsed, err := url.Parse(location)
	if err != nil {
		return "unknown"
	}
	return filepath.Base(parsed.Path)
}

// parseUploadResult checks the upload response for errors.
func parseUploadResult(status int, body []byte) error {
	if status == http.StatusConflict {
		return ErrDuplicate
	}
	if status >= 400 {
		return fmt.Errorf("upload failed (HTTP %d): %s", status,
			string(body[:min(300, len(body))]))
	}

	if status == http.StatusAccepted {
		return fmt.Errorf("Garmin accepted upload (HTTP 202); import completion is unconfirmed")
	}
	if status != http.StatusCreated {
		return fmt.Errorf("unexpected Garmin upload response (HTTP %d): %s", status,
			string(body[:min(300, len(body))]))
	}
	return parseImportBody(body)
}

func parseImportBody(body []byte) error {
	var result struct {
		DetailedImportResult struct {
			Failures  []interface{} `json:"failures"`
			Successes []interface{} `json:"successes"`
		} `json:"detailedImportResult"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("Garmin returned an unreadable import result: %w", err)
	}
	if len(result.DetailedImportResult.Failures) > 0 {
		return fmt.Errorf("upload reported failures: %v", result.DetailedImportResult.Failures)
	}
	if len(result.DetailedImportResult.Successes) == 0 {
		return fmt.Errorf("Garmin response contained no import successes; upload is unconfirmed")
	}

	return nil
}
