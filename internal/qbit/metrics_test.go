package qbit

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/guibibi/tordownloader/internal/store"
)

func TestMetricsEndpoint(t *testing.T) {
	srv, st := newTestServer(t)
	seedTorrent(t, st, store.Torrent{Infohash: "aa", Name: "a", State: store.StateTorBoxActive, DLSpeed: 100})
	seedTorrent(t, st, store.Torrent{Infohash: "bb", Name: "b", State: store.StateLocalDload, DLSpeed: 40})
	seedTorrent(t, st, store.Torrent{Infohash: "cc", Name: "c", State: store.StateError})

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	for _, want := range []string{
		`tordownloader_torrents{state="TORBOX_ACTIVE"} 1`,
		`tordownloader_torrents{state="ERROR"} 1`,
		`tordownloader_torrents{state="COMPLETE"} 0`,
		`tordownloader_torbox_slots_used 1`,
		`tordownloader_download_speed_bytes{stage="local"} 40`,
		`tordownloader_download_speed_bytes{stage="torbox"} 100`,
		`tordownloader_build_info{version="unknown"} 1`,
		`# TYPE tordownloader_failures_total counter`,
		`# TYPE tordownloader_downloaded_bytes_total counter`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}
