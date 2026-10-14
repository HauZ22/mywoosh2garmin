package garmin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestParseUploadResult(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
		wantDup bool
	}{
		{"ok", 201, `{"detailedImportResult":{"successes":[{"internalId":1}],"failures":[]}}`, false, false},
		{"accepted-is-not-complete", 202, ``, true, false},
		{"created-empty-body-is-unconfirmed", 201, ``, true, false},
		{"duplicate", 409, `{"detailedImportResult":{"failures":[{"messages":[{"code":202}]}]}}`, true, true},
		{"failure-in-body", 201, `{"detailedImportResult":{"failures":[{"messages":[{"code":500}]}]}}`, true, false},
		{"server-error", 500, `boom`, true, false},
		{"unauthorized", 401, `nope`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseUploadResult(tt.status, []byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrDuplicate); got != tt.wantDup {
				t.Errorf("errors.Is(ErrDuplicate) = %v, want %v", got, tt.wantDup)
			}
		})
	}
}

func TestUploadWaitsForGarminProcessing(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload-service/upload/fit":
			w.Header().Set("Location", "/upload-service/upload/status/123")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/upload-service/upload/status/123":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Location", "/activity-service/activity/456")
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, fmt.Sprintf("unexpected request: %s %s", r.Method, r.URL.Path), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient(t.TempDir())
	client.OAuth2 = &OAuth2Token{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	client.apiBaseURL = srv.URL
	client.httpClient = srv.Client()
	client.pollInterval = time.Millisecond

	file := t.TempDir() + "/activity.fit"
	if err := os.WriteFile(file, []byte("fit data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Upload(file); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if polls != 2 {
		t.Fatalf("poll count = %d, want 2", polls)
	}
}

func TestUploadRejectsAcceptedWithoutPollLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := NewClient(t.TempDir())
	client.OAuth2 = &OAuth2Token{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	client.apiBaseURL = srv.URL
	client.httpClient = srv.Client()

	file := t.TempDir() + "/activity.fit"
	if err := os.WriteFile(file, []byte("fit data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Upload(file); err == nil {
		t.Fatal("Upload() succeeded for an unconfirmed HTTP 202 response")
	}
}

func TestTokensRoundtripAndClear(t *testing.T) {
	dir := t.TempDir()
	o1 := &OAuth1Token{OAuthToken: "a", OAuthTokenSecret: "b", Domain: "garmin.com"}
	o2 := &OAuth2Token{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := SaveTokens(dir, o1, o2); err != nil {
		t.Fatal(err)
	}

	got1, got2, err := LoadTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got1.OAuthToken != "a" || got2.AccessToken != "tok" {
		t.Errorf("roundtrip mismatch: %+v %+v", got1, got2)
	}
	if got2.Expired() {
		t.Error("token should not be expired yet")
	}
	if (&OAuth2Token{ExpiresAt: time.Now().Add(-time.Minute).Unix()}).Expired() == false {
		t.Error("past token should be expired")
	}

	ClearTokens(dir)
	if _, _, err := LoadTokens(dir); err == nil {
		t.Error("expected error after ClearTokens")
	}
}
