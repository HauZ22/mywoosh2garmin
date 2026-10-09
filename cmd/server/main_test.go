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
	return &server{syncer: syncer, log: &logRing{}}
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
	s.tryLock()
	s.runSync(false)

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
	if !strings.Contains(rr.Body.String(), "MyWhoosh → Garmin") {
		t.Error("index page missing title")
	}
	// other paths 404
	rr = do(httptest.NewRequest(http.MethodGet, filepath.ToSlash(filepath.Join("/nope")), nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}
func postJSON(do func(*http.Request) *httptest.ResponseRecorder, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(req)
}

func TestMarkActivitiesPersistsAndCanBeUndone(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	rr := postJSON(do, "/api/activities/mark", `{"ids":["a1","a2"],"synced":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("mark status = %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]int
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["changed"] != 2 {
		t.Errorf("changed = %d, want 2", resp["changed"])
	}

	// A "restart": a second Syncer on the same data dir still knows both.
	restarted := core.New(s.syncer.DataDir(), core.Config{})
	for _, id := range []string{"a1", "a2"} {
		e, ok := restarted.Tracker().Entry(id)
		if !ok || e.Source != core.SourceManual {
			t.Errorf("%s: entry=%+v ok=%v, want manual marker", id, e, ok)
		}
	}

	rr = postJSON(do, "/api/activities/mark", `{"ids":["a1"],"synced":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("unmark status = %d", rr.Code)
	}
	if s.syncer.Tracker().IsSynced("a1") || !s.syncer.Tracker().IsSynced("a2") {
		t.Error("only a1 should have been unmarked")
	}
}

func TestMarkActivitiesValidatesBody(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	if rr := postJSON(do, "/api/activities/mark", `{"synced":true}`); rr.Code != http.StatusBadRequest {
		t.Errorf("missing ids: status = %d, want 400", rr.Code)
	}
	if rr := postJSON(do, "/api/activities/mark", `nope`); rr.Code != http.StatusBadRequest {
		t.Errorf("bad json: status = %d, want 400", rr.Code)
	}
}

func TestConfigKeepsAutoSyncWhenFieldOmitted(t *testing.T) {
	s := newTestServer(t)
	_, do := newTestMux(s)

	postJSON(do, "/api/config", `{"auto_sync_interval":30}`)
	cfg := s.syncer.Config()
	if cfg.AutoSyncInterval != 30 || cfg.AutoSyncActivatedAt == 0 {
		t.Fatalf("auto-sync not enabled: %+v", cfg)
	}
	activated := cfg.AutoSyncActivatedAt

	postJSON(do, "/api/config", `{"days":7}`)
	cfg = s.syncer.Config()
	if cfg.AutoSyncInterval != 30 || cfg.AutoSyncActivatedAt != activated || cfg.Days != 7 {
		t.Errorf("unrelated config change altered auto-sync: %+v", cfg)
	}

	postJSON(do, "/api/config", `{"auto_sync_interval":-5}`)
	if got := s.syncer.Config().AutoSyncInterval; got != 0 {
		t.Errorf("negative interval = %d, want 0 (off)", got)
	}
}

func TestBasicAuth(t *testing.T) {
	s := newTestServer(t)
	s.authUser, s.authPass = "admin", "s3cret"
	mux := http.NewServeMux()
	s.routes(mux)
	h := s.withAuth(mux)

	do := func(path, user, pass string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := do("/api/status", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no credentials: %d, want 401", code)
	}
	if code := do("/api/status", "admin", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d, want 401", code)
	}
	if code := do("/api/status", "admin", "s3cret"); code != http.StatusOK {
		t.Errorf("right credentials: %d, want 200", code)
	}
	if code := do("/healthz", "", ""); code != http.StatusOK {
		t.Errorf("/healthz must stay open for Docker: %d", code)
	}

	// Without a password the middleware is a no-op.
	open := newTestServer(t)
	mux2 := http.NewServeMux()
	open.routes(mux2)
	rr := httptest.NewRecorder()
	open.withAuth(mux2).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("auth disabled: %d, want 200", rr.Code)
	}
}

func TestLoadConfigEnvDoesNotWipeStoredPassword(t *testing.T) {
	dir := t.TempDir()
	if err := core.SaveConfig(dir, core.Config{GarminEmail: "old@x.io", GarminPassword: "stored-pw"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARMIN_EMAIL", "new@x.io")
	t.Setenv("GARMIN_PASSWORD", "")
	t.Setenv("SYNC_DAYS", "21")

	cfg := loadConfig(dir)
	if cfg.GarminEmail != "new@x.io" || cfg.GarminPassword != "stored-pw" || cfg.Days != 21 {
		t.Errorf("unexpected merge: %+v", cfg)
	}
}

func TestProbe(t *testing.T) {
	s := newTestServer(t)
	mux := http.NewServeMux()
	s.routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	if code := probe(strings.TrimPrefix(ts.URL, "http://")); code != 0 {
		t.Errorf("probe of healthy server = %d, want 0", code)
	}
	ts.Close()
	if code := probe(strings.TrimPrefix(ts.URL, "http://")); code != 1 {
		t.Errorf("probe of stopped server = %d, want 1", code)
	}
}
