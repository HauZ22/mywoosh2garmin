package garmin

import (
	"errors"
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
		{"ok-empty-body", 202, ``, false, false},
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
