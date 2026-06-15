package qbit

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

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

func TestUITorrentsTorBoxPhaseFields(t *testing.T) {
	srv, st := newTestServer(t)
	seedTorrent(t, st, store.Torrent{
		Infohash: strings.Repeat("b", 40), Name: "Movie", Size: 2000,
		State: store.StateTorBoxActive, TorBoxProgress: 0.3,
		Seeds: 12, Peers: 4, ETA: 300, Cached: true,
	})

	var got uiResponse
	_, body := get(t, srv, "/ui/torrents")
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Torrents) != 1 {
		t.Fatalf("torrents = %d, want 1", len(got.Torrents))
	}
	ut := got.Torrents[0]
	if ut.Phase != "torbox" {
		t.Errorf("phase = %q, want torbox", ut.Phase)
	}
	if ut.Seeds != 12 || ut.Peers != 4 {
		t.Errorf("seeds/peers = %d/%d, want 12/4", ut.Seeds, ut.Peers)
	}
	// In the TorBox phase the ETA is TorBox's own estimate, not a disk-speed derivation.
	if ut.ETA != 300 {
		t.Errorf("eta = %d, want 300 (TorBox-side)", ut.ETA)
	}
	if !ut.Cached {
		t.Errorf("cached = false, want true")
	}
}

func TestUITorrentsStallCountdownPayload(t *testing.T) {
	srv, st := newTestServer(t) // newTestServer configures a 10m stall timeout.
	id := seedTorrent(t, st, store.Torrent{
		Infohash: strings.Repeat("c", 40), Name: "Stalling", Size: 1000,
		State: store.StateTorBoxActive, TorBoxProgress: 0.1,
	})
	// A frozen stall clock from 8 minutes ago.
	froze := time.Now().Add(-8 * time.Minute).Unix()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE torrents SET progress_at = ?, torbox_state = 'downloading' WHERE id = ?`, froze, id); err != nil {
		t.Fatalf("freeze progress_at: %v", err)
	}

	var got uiResponse
	_, body := get(t, srv, "/ui/torrents")
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The dashboard derives the countdown from these; assert they reach the client.
	if got.StallTimeout != 600 {
		t.Errorf("stall_timeout = %d, want 600", got.StallTimeout)
	}
	ut := got.Torrents[0]
	if ut.ProgressAt != froze {
		t.Errorf("progress_at = %d, want %d", ut.ProgressAt, froze)
	}
	if ut.TorBoxState != "downloading" {
		t.Errorf("torbox_state = %q, want downloading", ut.TorBoxState)
	}
}

func TestUIFiles(t *testing.T) {
	srv, st := newTestServer(t)
	hash := strings.Repeat("d", 40)
	id := seedTorrent(t, st, store.Torrent{
		Infohash: hash, Name: "Pack S01", Size: 300, State: store.StateLocalDload,
	})
	ctx := context.Background()
	for _, f := range []struct {
		rel, short string
		size       int64
		done       int
	}{
		{"Pack S01/e01.mkv", "e01.mkv", 100, 1},
		{"Pack S01/e02.mkv", "e02.mkv", 200, 0},
	} {
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO files (torrent_id, rel_path, short_name, size, done) VALUES (?,?,?,?,?)`,
			id, f.rel, f.short, f.size, f.done); err != nil {
			t.Fatalf("insert file: %v", err)
		}
	}

	resp, body := get(t, srv, "/ui/torrents/files?hash="+hash)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got uiFilesResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(got.Files))
	}
	if got.Files[0].Name != "e01.mkv" || got.Files[0].Size != 100 || !got.Files[0].Done {
		t.Errorf("file[0] = %+v, want e01.mkv/100/done", got.Files[0])
	}
	if got.Files[1].Done {
		t.Errorf("file[1] should be pending, got done")
	}
	if got.Files[1].Path != "Pack S01/e02.mkv" {
		t.Errorf("file[1] path = %q, want full rel path", got.Files[1].Path)
	}

	// Unknown hash 404s; missing hash is a 400.
	if r, _ := get(t, srv, "/ui/torrents/files?hash="+strings.Repeat("e", 40)); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown hash status = %d, want 404", r.StatusCode)
	}
	if r, _ := get(t, srv, "/ui/torrents/files"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("missing hash status = %d, want 400", r.StatusCode)
	}
}
