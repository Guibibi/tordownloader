package qbit

import (
	"testing"

	"github.com/guibibi/tordownloader/internal/store"
)

func TestQbitState(t *testing.T) {
	tests := []struct {
		name string
		tor  store.Torrent
		want string
	}{
		{"queued", store.Torrent{State: store.StateQueued}, "stalledDL"},
		{"active no progress", store.Torrent{State: store.StateTorBoxActive}, "stalledDL"},
		{"active with progress", store.Torrent{State: store.StateTorBoxActive, TorBoxProgress: 0.3}, "downloading"},
		{"local queued", store.Torrent{State: store.StateLocalQueued}, "downloading"},
		{"local download", store.Torrent{State: store.StateLocalDload}, "downloading"},
		{"complete", store.Torrent{State: store.StateComplete}, "pausedUP"},
		{"error", store.Torrent{State: store.StateError}, "error"},
		{"unknown", store.Torrent{State: "WAT"}, "stalledDL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qbitState(tt.tor); got != tt.want {
				t.Errorf("qbitState(%q) = %q, want %q", tt.tor.State, got, tt.want)
			}
		})
	}
}

func TestBlendedProgress(t *testing.T) {
	tests := []struct {
		name string
		tor  store.Torrent
		want float64
	}{
		{"half torbox", store.Torrent{State: store.StateTorBoxActive, TorBoxProgress: 1}, 0.5},
		{"half local", store.Torrent{State: store.StateLocalDload, LocalProgress: 1}, 0.5},
		{"both half", store.Torrent{State: store.StateLocalDload, TorBoxProgress: 0.5, LocalProgress: 0.5}, 0.5},
		{"complete forces 1", store.Torrent{State: store.StateComplete, TorBoxProgress: 0, LocalProgress: 0}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blendedProgress(tt.tor); got != tt.want {
				t.Errorf("blendedProgress = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenderTorrentFields(t *testing.T) {
	tor := store.Torrent{
		Infohash:      "abc123",
		Name:          "Show.S01E01",
		Category:      "tv-sonarr",
		SavePath:      "/downloads/tv-sonarr",
		ContentPath:   "/downloads/tv-sonarr/Show.S01E01",
		Size:          1000,
		State:         store.StateLocalDload,
		LocalProgress: 0.5,
		DLSpeed:       100,
		AddedOn:       42,
	}
	got := renderTorrent(tor)

	if got.Hash != "abc123" {
		t.Errorf("hash = %q", got.Hash)
	}
	if got.Completed != 500 {
		t.Errorf("completed = %d, want 500", got.Completed)
	}
	if got.AmountLeft != 500 {
		t.Errorf("amount_left = %d, want 500", got.AmountLeft)
	}
	if got.ETA != 5 { // 500 bytes left / 100 B/s
		t.Errorf("eta = %d, want 5", got.ETA)
	}
	if got.State != "downloading" {
		t.Errorf("state = %q", got.State)
	}
}

func TestRenderTorrentUnknownETA(t *testing.T) {
	got := renderTorrent(store.Torrent{State: store.StateQueued, Size: 1000})
	if got.ETA != etaUnknown {
		t.Errorf("eta = %d, want %d", got.ETA, etaUnknown)
	}
}
