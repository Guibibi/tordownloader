package qbit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
)

// testPolicy is the failure policy handlers are exercised against: the shipped
// defaults, so the dashboard payload assertions reflect a realistic config.
func testPolicy() FailurePolicy {
	return FailurePolicy{
		StallTimeout:         10 * time.Minute,
		ProgressStallTimeout: 2 * time.Hour,
		CachedStallTimeout:   30 * time.Minute,
		MinSpeed:             50 << 10,
		SlowWindow:           15 * time.Minute,
	}
}

// newTestServer opens a fresh on-disk store in a temp dir and returns an
// httptest server fronting the qbit routes, plus the store for seeding rows.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := httptest.NewServer(New(st, "/downloads", testPolicy(), nil, nil).Routes())
	t.Cleanup(srv.Close)
	return srv, st
}

// seedTorrent inserts a torrent row directly (write helpers arrive in M3).
func seedTorrent(t *testing.T, st *store.Store, tor store.Torrent) int64 {
	t.Helper()
	res, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO torrents (infohash, name, category, save_path, content_path, size,
			state, torbox_progress, local_progress, dlspeed, seeds, peers, eta, cached,
			added_on, completed_on)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tor.Infohash, tor.Name, tor.Category, tor.SavePath, tor.ContentPath, tor.Size,
		tor.State, tor.TorBoxProgress, tor.LocalProgress, tor.DLSpeed,
		tor.Seeds, tor.Peers, tor.ETA, tor.Cached, tor.AddedOn, tor.CompletedOn)
	if err != nil {
		t.Fatalf("seed torrent: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestLogin(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.PostForm(srv.URL+"/api/v2/auth/login",
		map[string][]string{"username": {"admin"}, "password": {"x"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "Ok." {
		t.Errorf("body = %q, want Ok.", body)
	}
	var sid bool
	for _, c := range resp.Cookies() {
		if c.Name == "SID" && c.Value != "" {
			sid = true
		}
	}
	if !sid {
		t.Error("expected SID cookie")
	}
}

func TestAppVersionEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	if _, body := get(t, srv, "/api/v2/app/version"); body != appVersion {
		t.Errorf("version = %q, want %q", body, appVersion)
	}
	if _, body := get(t, srv, "/api/v2/app/webapiVersion"); body != webAPIVersion {
		t.Errorf("webapiVersion = %q, want %q", body, webAPIVersion)
	}
}

func TestAppPreferences(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := get(t, srv, "/api/v2/app/preferences")
	var prefs map[string]any
	if err := json.Unmarshal([]byte(body), &prefs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if prefs["save_path"] != "/downloads" {
		t.Errorf("save_path = %v, want /downloads", prefs["save_path"])
	}
}

func TestTorrentsInfoEmptyIsArray(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, body := get(t, srv, "/api/v2/torrents/info")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("empty list body = %q, want []", body)
	}
}

func TestTorrentsInfoRendersAndFilters(t *testing.T) {
	srv, st := newTestServer(t)
	seedTorrent(t, st, store.Torrent{
		Infohash: "aaa", Name: "A", Category: "tv-sonarr", Size: 100,
		State: store.StateComplete, LocalProgress: 1, TorBoxProgress: 1,
		SavePath: "/downloads/tv-sonarr", CompletedOn: 99,
	})
	seedTorrent(t, st, store.Torrent{
		Infohash: "bbb", Name: "B", Category: "radarr", Size: 200,
		State: store.StateQueued,
	})

	// No filter → both.
	_, body := get(t, srv, "/api/v2/torrents/info")
	var all []map[string]any
	if err := json.Unmarshal([]byte(body), &all); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d torrents, want 2", len(all))
	}

	// Category filter.
	_, body = get(t, srv, "/api/v2/torrents/info?category=tv-sonarr")
	var tv []map[string]any
	_ = json.Unmarshal([]byte(body), &tv)
	if len(tv) != 1 || tv[0]["hash"] != "aaa" {
		t.Fatalf("category filter = %v", tv)
	}
	if tv[0]["state"] != "pausedUP" {
		t.Errorf("complete state = %v, want pausedUP", tv[0]["state"])
	}

	// Hashes filter.
	_, body = get(t, srv, "/api/v2/torrents/info?hashes=bbb")
	var h []map[string]any
	_ = json.Unmarshal([]byte(body), &h)
	if len(h) != 1 || h[0]["hash"] != "bbb" {
		t.Fatalf("hashes filter = %v", h)
	}
}

func TestTorrentsFiles(t *testing.T) {
	srv, st := newTestServer(t)
	id := seedTorrent(t, st, store.Torrent{Infohash: "ccc", Name: "C", Size: 100})
	_, err := st.DB().Exec(
		`INSERT INTO files (torrent_id, torbox_file_id, rel_path, short_name, size, downloaded, done)
		 VALUES (?,?,?,?,?,?,?)`, id, 0, "C/c.mkv", "c.mkv", 100, 50, 0)
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, body := get(t, srv, "/api/v2/torrents/files?hash=ccc")
	var files []map[string]any
	if err := json.Unmarshal([]byte(body), &files); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0]["name"] != "C/c.mkv" {
		t.Errorf("name = %v, want C/c.mkv", files[0]["name"])
	}
	if files[0]["progress"] != 0.5 {
		t.Errorf("progress = %v, want 0.5", files[0]["progress"])
	}
}

func TestTorrentsFilesUnknownHash(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, _ := get(t, srv, "/api/v2/torrents/files?hash=nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCategories(t *testing.T) {
	srv, st := newTestServer(t)
	if _, err := st.DB().Exec(`INSERT INTO categories (name, save_path) VALUES (?,?)`,
		"tv-sonarr", "/downloads/tv-sonarr"); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	_, body := get(t, srv, "/api/v2/torrents/categories")
	var cats map[string]map[string]string
	if err := json.Unmarshal([]byte(body), &cats); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c, ok := cats["tv-sonarr"]
	if !ok {
		t.Fatalf("missing category, got %v", cats)
	}
	if c["savePath"] != "/downloads/tv-sonarr" {
		t.Errorf("savePath = %q", c["savePath"])
	}
}

func TestTransferInfo(t *testing.T) {
	srv, st := newTestServer(t)
	seedTorrent(t, st, store.Torrent{Infohash: "ddd", State: store.StateLocalDload, DLSpeed: 1500})
	_, body := get(t, srv, "/api/v2/transfer/info")
	var info map[string]any
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if info["connection_status"] != "connected" {
		t.Errorf("connection_status = %v", info["connection_status"])
	}
	if info["dl_info_speed"].(float64) != 1500 {
		t.Errorf("dl_info_speed = %v, want 1500", info["dl_info_speed"])
	}
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, body := get(t, srv, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}
