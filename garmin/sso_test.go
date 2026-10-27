package garmin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSSO simulates the Garmin SSO pages. With mfa set, the credential POST
// lands on the MFA page and only wantCode completes the login.
func fakeSSO(t *testing.T, mfa bool, wantCode string) *httptest.Server {
	t.Helper()
	page := func(title, extra string) string {
		return fmt.Sprintf(`<html><head><title>%s</title></head><body>%s</body></html>`, title, extra)
	}
	form := func(csrf string) string { return `<input type="hidden" name="_csrf" value="` + csrf + `"/>` }
	success := page("Success", `<a href="https://sso.garmin.com/sso/embed?ticket=ST-12345-abc">x</a>`)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sso/embed":
			http.SetCookie(w, &http.Cookie{Name: "GARMIN-SSO", Value: "1", Path: "/"})
		case r.Method == http.MethodGet && r.URL.Path == "/sso/signin":
			fmt.Fprint(w, page("GARMIN Authentication Application", form("csrf-login")))
		case r.Method == http.MethodPost && r.URL.Path == "/sso/signin":
			if r.FormValue("_csrf") != "csrf-login" || r.FormValue("password") != "pw" {
				fmt.Fprint(w, page("GARMIN Authentication Application", "bad credentials"))
				return
			}
			if mfa {
				fmt.Fprint(w, page("Enter MFA code for login", form("csrf-mfa")))
				return
			}
			fmt.Fprint(w, success)
		case r.Method == http.MethodPost && r.URL.Path == "/sso/verifyMFA/loginEnterMfaCode":
			if _, err := r.Cookie("GARMIN-SSO"); err != nil {
				t.Errorf("MFA post lost the SSO session cookie")
			}
			if r.FormValue("_csrf") != "csrf-mfa" || r.FormValue("fromPage") != "setupEnterMfaCode" {
				t.Errorf("unexpected MFA form: %v", r.Form)
			}
			if r.FormValue("mfa-code") != wantCode {
				fmt.Fprint(w, page("Enter MFA code for login", form("csrf-mfa")))
				return
			}
			fmt.Fprint(w, success)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestSignInWithoutMFA(t *testing.T) {
	srv := fakeSSO(t, false, "")
	defer srv.Close()

	prompted := false
	ticket, err := signIn(newSSOSession(), srv.URL, "a@b.c", "pw", func() (string, error) {
		prompted = true
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticket != "ST-12345-abc" {
		t.Errorf("ticket = %q", ticket)
	}
	if prompted {
		t.Error("MFA prompt called for an account without 2FA")
	}
}

func TestSignInWithMFA(t *testing.T) {
	srv := fakeSSO(t, true, "654321")
	defer srv.Close()

	ticket, err := signIn(newSSOSession(), srv.URL, "a@b.c", "pw", func() (string, error) {
		return " 654 321 ", nil // whitespace as pasted from an e-mail is stripped
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticket != "ST-12345-abc" {
		t.Errorf("ticket = %q", ticket)
	}
}

func TestSignInMFAErrors(t *testing.T) {
	srv := fakeSSO(t, true, "654321")
	defer srv.Close()

	tests := []struct {
		name   string
		prompt MFAPrompt
		want   string
	}{
		{"no-prompt", nil, ""},
		{"wrong-code", func() (string, error) { return "000000", nil }, "abgelehnt"},
		{"empty-code", func() (string, error) { return "  ", nil }, "kein Code"},
		{"cancelled", func() (string, error) { return "", errors.New("cancelled") }, "cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := signIn(newSSOSession(), srv.URL, "a@b.c", "pw", tt.prompt)
			if err == nil {
				t.Fatal("signIn succeeded")
			}
			if tt.prompt == nil && !errors.Is(err, ErrMFARequired) {
				t.Errorf("err = %v, want ErrMFARequired", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestSignInWrongPassword(t *testing.T) {
	srv := fakeSSO(t, true, "654321")
	defer srv.Close()

	_, err := signIn(newSSOSession(), srv.URL, "a@b.c", "wrong", func() (string, error) {
		t.Error("MFA prompt called after a wrong password")
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "login failed") {
		t.Fatalf("err = %v, want login failed", err)
	}
}
