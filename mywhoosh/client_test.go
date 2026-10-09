package mywhoosh

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI serves a newest-first activity list split into pages of two.
func fakeAPI(t *testing.T, ages []time.Duration, pagesFetched *int32) *Client {
	t.Helper()

	const perPage = 2
	totalPages := (len(ages) + perPage - 1) / perPage

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/rider/profile/activities", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(pagesFetched, 1)
		var req struct{ Page int }
		_ = json.NewDecoder(r.Body).Decode(&req)

		var results []map[string]interface{}
		for i := (req.Page - 1) * perPage; i < req.Page*perPage && i < len(ages); i++ {
			results = append(results, map[string]interface{}{
				"id":    fmt.Sprintf("act-%d", i),
				"title": fmt.Sprintf("Ride %d", i),
				"date":  float64(time.Now().Add(-ages[i]).Unix()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"results": results, "totalPages": totalPages},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldBase := activitiesBase
	activitiesBase = srv.URL + "/v2/"
	t.Cleanup(func() { activitiesBase = oldBase })

	c := NewClient("")
	c.token = "tok"
	return c
}

func TestGetRecentActivitiesStopsPagingAtCutoff(t *testing.T) {
	day := 24 * time.Hour
	var pages int32
	// 6 activities over 3 pages; only the first 3 are within 10 days.
	c := fakeAPI(t, []time.Duration{1 * day, 2 * day, 5 * day, 30 * day, 60 * day, 90 * day}, &pages)

	got, err := c.GetRecentActivities(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d activities, want 3", len(got))
	}
	if got[0].ID != "act-0" || got[2].ID != "act-2" {
		t.Errorf("unexpected order/ids: %v, %v", got[0].ID, got[2].ID)
	}
	if n := atomic.LoadInt32(&pages); n != 2 {
		t.Errorf("fetched %d pages, want 2 (page 3 must not be requested)", n)
	}
}

func TestGetRecentActivitiesWalksAllPagesWhenAllRecent(t *testing.T) {
	var pages int32
	c := fakeAPI(t, []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}, &pages)

	got, err := c.GetRecentActivities(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || atomic.LoadInt32(&pages) != 2 {
		t.Errorf("got %d activities in %d pages, want 3 in 2", len(got), pages)
	}
}

func TestRequestsNeedLogin(t *testing.T) {
	c := NewClient("")
	if _, err := c.GetRecentActivities(1); err == nil {
		t.Error("expected error without token")
	}
	if _, err := c.DownloadFitFile("x"); err == nil {
		t.Error("expected error without token")
	}
}

func TestFormattedDuration(t *testing.T) {
	cases := map[string]string{
		"01:31:54.000": "1h31m54s",
		"00:45:10.000": "45m10s",
		"":             "",
	}
	for in, want := range cases {
		a := Activity{RideDuration: in}
		if got := a.FormattedDuration(); got != want {
			t.Errorf("FormattedDuration(%q) = %q, want %q", in, got, want)
		}
	}
}
