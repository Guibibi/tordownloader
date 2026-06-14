package qbit

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/guibibi/tordownloader/internal/store"
)

func TestUIIndex(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, body := get(t, srv, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "/ui/torrents") {
		t.Errorf("page should reference the data endpoint")
	}

	// Unknown paths fall through to the catch-all and must 404, not serve HTML.
	if resp, _ := get(t, srv, "/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d for /nope, want 404", resp.StatusCode)
	}
}

func TestUITorrents(t *testing.T) {
	srv, st := newTestServer(t)
	seedTorrent(t, st, store.Torrent{
		Infohash: strings.Repeat("a", 40), Name: "Show S01", Size: 1000,
		State: store.StateLocalDload, TorBoxProgress: 1, LocalProgress: 0.25, DLSpeed: 100,
	})

	resp, body := get(t, srv, "/ui/torrents")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got uiResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DLSpeed != 100 {
		t.Errorf("dlspeed = %d, want 100", got.DLSpeed)
	}
	if len(got.Torrents) != 1 {
		t.Fatalf("torrents = %d, want 1", len(got.Torrents))
	}
	ut := got.Torrents[0]
	if ut.Phase != "local" {
		t.Errorf("phase = %q, want local", ut.Phase)
	}
	// Blended: 0.5*1 + 0.5*0.25 = 0.625.
	if ut.Progress < 0.62 || ut.Progress > 0.63 {
		t.Errorf("progress = %v, want ~0.625", ut.Progress)
	}
	// 750 bytes left / 100 B/s = 7s.
	if ut.ETA != 7 {
		t.Errorf("eta = %d, want 7", ut.ETA)
	}
}
