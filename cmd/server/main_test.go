package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mywhoosh2garmin/internal/core"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	syncer := core.New(dir, core.Config{})
	s := &server{dataDir: dir, cfg: syncer.Config(), syncer: syncer, log: &logRing{}}
	return s
}

func newTestMux(s *server) (*http.ServeMux, func(*http.Request) *httptest.ResponseRecorder) {
	mux := http.NewServeMux()
	s.routes(mux)
	handler := mux
	return mux, func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, r)
		return rr
	}
}

func TestStatusEndpoint(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	rr := do(httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d", rr.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["busy"] != false {
		t.Errorf("busy = %v, want false", body["busy"])
	}
	cfg, ok := body["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("config field missing: %v", body)
	}
	if cfg["garmin_configured"] != false {
		t.Errorf("garmin_configured = %v, want false", cfg["garmin_configured"])
	}
	if _, ok := body["log"].([]interface{}); !ok {
		t.Errorf("log field missing")
	}
}

func TestConfigEndpoint(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	body := `{"mywhoosh_email":"mw@x.io","mywhoosh_password":"pw-mw","garmin_email":"ga@x.io","garmin_password":"pw-ga","days":4}`
	rr := do(httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("config status = %d: %s", rr.Code, rr.Body.String())
	}

	rr = do(httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &status)
	cfg := status["config"].(map[string]interface{})
	if cfg["garmin_configured"] != true {
		t.Errorf("garmin_configured = %v, want true", cfg["garmin_configured"])
	}
	if cfg["days"] != float64(4) {
		t.Errorf("days = %v, want 4", cfg["days"])
	}
	// passwords must not leak in plaintext
	if strings.Contains(rr.Body.String(), "pw-ga") || strings.Contains(rr.Body.String(), "pw-mw") {
		t.Errorf("password leaked in status response")
	}
}

func TestSyncConflictWhenBusy(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	rr := do(httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("sync status = %d", rr.Code)
	}

	// Force busy state so a second job is rejected deterministically.
	s.mu.Lock()
	s.busy = true
	s.mu.Unlock()

	rr = do(httptest.NewRequest(http.MethodPost, "/api/sync", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 while busy, got %d", rr.Code)
	}

	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

func TestRunSyncWithoutCredentialsFailsGracefully(t *testing.T) {
	s := newTestServer(t)
	s.runSync()

	s.mu.Lock()
	sum := s.lastSummary
	s.mu.Unlock()
	if sum.Total != 0 {
		t.Errorf("total = %d, want 0", sum.Total)
	}
	if sum.Error == "" {
		t.Error("expected an error for missing MyWhoosh credentials")
	}
	if !strings.Contains(strings.Join(s.log.tail(50), "\n"), "gestartet") {
		t.Error("missing log lines")
	}
}

func TestUploadMultipartAccepted(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("file", "workout.tcx")
	part.Write([]byte("<TrainingCenterDatabase/>"))
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := do(req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("upload status = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUploadRawBodyNeedsFilenameHeader(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	rr := do(httptest.NewRequest(http.MethodPost, "/api/webhook/upload", strings.NewReader("data")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without X-Filename, got %d", rr.Code)
	}
}

func TestUploadRejectsWrongField(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("other", "x")
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := do(req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestStaticIndexServed(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)
	rr := do(httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("index status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "FIT → Garmin Sync") {
		t.Error("index page missing title")
	}
	// other paths 404
	rr = do(httptest.NewRequest(http.MethodGet, filepath.ToSlash(filepath.Join("/nope")), nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}