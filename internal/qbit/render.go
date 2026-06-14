package qbit

import (
	"github.com/guibibi/tordownloader/internal/store"
)

// etaUnknown is qBittorrent's sentinel for "no ETA" (100 days in seconds).
const etaUnknown = 8640000

// torrentInfo is the qBittorrent torrent object returned by torrents/info. It
// carries the fields Sonarr/Radarr read (docs/API_REFERENCE.md §2) plus a few
// common extras real qBittorrent includes, so clients parsing the full object
// don't choke.
type torrentInfo struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	TotalSize    int64   `json:"total_size"`
	Progress     float64 `json:"progress"`
	DLSpeed      int64   `json:"dlspeed"`
	UpSpeed      int64   `json:"upspeed"`
	ETA          int64   `json:"eta"`
	State        string  `json:"state"`
	Category     string  `json:"category"`
	Tags         string  `json:"tags"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	Completed    int64   `json:"completed"`
	AmountLeft   int64   `json:"amount_left"`
	Downloaded   int64   `json:"downloaded"`
	CompletionOn int64   `json:"completion_on"`
	AddedOn      int64   `json:"added_on"`
	Ratio        float64 `json:"ratio"`
	SeedingTime  int64   `json:"seeding_time"`
	NumSeeds     int     `json:"num_seeds"`
	NumLeechs    int     `json:"num_leechs"`
	Priority     int     `json:"priority"`
	ForceStart   bool    `json:"force_start"`
	AutoTMM      bool    `json:"auto_tmm"`
	Availability float64 `json:"availability"`
}

// renderTorrent maps a stored torrent to the qBittorrent object.
func renderTorrent(t store.Torrent) torrentInfo {
	progress := blendedProgress(t)
	completed := int64(t.LocalProgress * float64(t.Size))
	if completed > t.Size {
		completed = t.Size
	}
	amountLeft := t.Size - completed

	eta := int64(etaUnknown)
	if t.DLSpeed > 0 && amountLeft > 0 {
		eta = amountLeft / t.DLSpeed
	}

	contentPath := t.ContentPath
	if contentPath == "" {
		contentPath = t.SavePath
	}

	return torrentInfo{
		Hash:         t.Infohash,
		Name:         t.Name,
		Size:         t.Size,
		TotalSize:    t.Size,
		Progress:     progress,
		DLSpeed:      t.DLSpeed,
		ETA:          eta,
		State:        qbitState(t),
		Category:     t.Category,
		SavePath:     t.SavePath,
		ContentPath:  contentPath,
		Completed:    completed,
		AmountLeft:   amountLeft,
		Downloaded:   completed,
		CompletionOn: t.CompletedOn,
		AddedOn:      t.AddedOn,
		Priority:     1,
		Availability: -1,
	}
}

// blendedProgress combines TorBox-side and local progress into the single 0..1
// bar Sonarr sees: the first half is TorBox fetching, the second is the local
// download (docs/API_REFERENCE.md §2).
func blendedProgress(t store.Torrent) float64 {
	if t.State == store.StateComplete {
		return 1
	}
	p := 0.5*t.TorBoxProgress + 0.5*t.LocalProgress
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// qbitState derives the qBittorrent state string from the internal state
// (docs/API_REFERENCE.md §3). Unknown states report stalledDL, the choice that
// keeps Sonarr waiting rather than giving up.
func qbitState(t store.Torrent) string {
	switch t.State {
	case store.StateQueued:
		return "stalledDL"
	case store.StateTorBoxActive:
		// TorBox fetching: "downloading" once there's progress, else stalled.
		if t.TorBoxProgress > 0 {
			return "downloading"
		}
		return "stalledDL"
	case store.StateLocalQueued, store.StateLocalDload:
		return "downloading"
	case store.StateComplete:
		return "pausedUP" // an "UP" state → Sonarr imports (never *DL)
	case store.StateError:
		return "error"
	default:
		return "stalledDL"
	}
}
