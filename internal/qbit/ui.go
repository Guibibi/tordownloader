package qbit

import (
	_ "embed"
	"net/http"
	"strings"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
)

// uiPage is the self-contained status dashboard. It polls /ui/torrents and
// renders progress; no build step, no external assets.
//
//go:embed ui.html
var uiPage []byte

// uiTorrent is the dashboard view of a torrent. Unlike the qBittorrent object
// (which blends progress into a single bar for Sonarr), this exposes the TorBox
// and local phases separately so the page can show what's actually happening.
type uiTorrent struct {
	Hash           string  `json:"hash"`
	Name           string  `json:"name"`
	Size           int64   `json:"size"`
	State          string  `json:"state"`
	StateLabel     string  `json:"state_label"`
	TorBoxState    string  `json:"torbox_state"`
	Phase          string  `json:"phase"`
	TorBoxProgress float64 `json:"torbox_progress"`
	LocalProgress  float64 `json:"local_progress"`
	Progress       float64 `json:"progress"`
	DLSpeed        int64   `json:"dlspeed"`
	ETA            int64   `json:"eta"`
	Seeds          int     `json:"seeds"`
	Peers          int     `json:"peers"`
	Cached         bool    `json:"cached"`
	Error          string  `json:"error"`
	Category       string  `json:"category"`
	AddedOn        int64   `json:"added_on"`
	// ProgressAt is the last time the TorBox fetch advanced (the stall clock). The
	// dashboard pairs it with StallTimeout to show how long a stalled fetch has
	// left before it's failed.
	ProgressAt int64 `json:"progress_at"`
	// SpeedAt/SpeedBytes are the open average-speed window: when it started and
	// the byte count it started from. With the live progress the dashboard can
	// show the same average the engine will judge the torrent on.
	SpeedAt    int64 `json:"speed_at"`
	SpeedBytes int64 `json:"speed_bytes"`
}

// uiResponse is the payload the dashboard polls.
type uiResponse struct {
	Torrents []uiTorrent `json:"torrents"`
	DLSpeed  int64       `json:"dlspeed"`
	Version  string      `json:"version"`
	// StallTimeout is the configured failure.stall_timeout in seconds — the grace
	// for a fetch stalled at 0%; 0 means stall-failing is disabled for that tier
	// and the dashboard shows no countdown.
	StallTimeout int64 `json:"stall_timeout"`
	// ProgressStallTimeout is the configured failure.progress_stall_timeout in
	// seconds, used for the countdown on fetches that have made real progress;
	// 0 disables it for them.
	ProgressStallTimeout int64 `json:"progress_stall_timeout"`
	// CachedStallTimeout is the configured failure.cached_stall_timeout in seconds,
	// used for the countdown on cached torrents; 0 disables it for them.
	CachedStallTimeout int64 `json:"cached_stall_timeout"`
	// MinSpeed is the configured failure.min_speed in bytes/s — the average a
	// moving TorBox fetch must sustain over SlowWindow before it is abandoned.
	// 0 means the check is off; the dashboard then shows no speed floor.
	MinSpeed int64 `json:"min_speed"`
	// SlowWindow is the configured failure.slow_window in seconds, the window
	// MinSpeed is averaged over.
	SlowWindow int64 `json:"slow_window"`
}

// uiIndex serves the dashboard HTML at /.
func (h *Handler) uiIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiPage)
}

// uiTorrents returns the dashboard data as JSON, newest first.
func (h *Handler) uiTorrents(w http.ResponseWriter, r *http.Request) {
	torrents, err := h.store.ListTorrents(r.Context(), store.TorrentFilter{})
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	out := make([]uiTorrent, 0, len(torrents))
	var totalDL int64
	for _, t := range torrents {
		totalDL += t.DLSpeed

		phase := uiPhase(t.State)

		// ETA is phase-specific: while TorBox is fetching, use its server-side
		// estimate; while downloading to disk, derive it from the remaining bytes
		// and the current local speed. -1 means unknown.
		eta := int64(-1)
		if phase == "torbox" {
			if t.ETA > 0 {
				eta = t.ETA
			}
		} else {
			completed := int64(t.LocalProgress * float64(t.Size))
			if completed > t.Size {
				completed = t.Size
			}
			if t.DLSpeed > 0 && t.Size > completed {
				eta = (t.Size - completed) / t.DLSpeed
			}
		}

		out = append(out, uiTorrent{
			Hash:           t.Infohash,
			Name:           t.Name,
			Size:           t.Size,
			State:          t.State,
			StateLabel:     uiStateLabel(t),
			TorBoxState:    t.TorBoxState,
			Phase:          phase,
			TorBoxProgress: clamp01(t.TorBoxProgress),
			LocalProgress:  clamp01(t.LocalProgress),
			Progress:       blendedProgress(t),
			DLSpeed:        t.DLSpeed,
			ETA:            eta,
			Seeds:          t.Seeds,
			Peers:          t.Peers,
			Cached:         t.Cached,
			Error:          t.Error,
			Category:       t.Category,
			AddedOn:        t.AddedOn,
			ProgressAt:     t.ProgressAt,
			SpeedAt:        t.SpeedAt,
			SpeedBytes:     t.SpeedBytes,
		})
	}

	h.writeJSON(w, r, uiResponse{
		Torrents:             out,
		DLSpeed:              totalDL,
		Version:              appVersion,
		StallTimeout:         int64(h.failure.StallTimeout / time.Second),
		ProgressStallTimeout: int64(h.failure.ProgressStallTimeout / time.Second),
		CachedStallTimeout:   int64(h.failure.CachedStallTimeout / time.Second),
		MinSpeed:             h.failure.MinSpeed,
		SlowWindow:           int64(h.failure.SlowWindow / time.Second),
	})
}

// uiFile is the dashboard view of one file within a torrent. Per-file progress is
// binary: Done flips when the file finishes and verifies (Downloaded isn't
// tracked mid-stream), so the dashboard renders a done/pending list rather than
// per-file bars.
type uiFile struct {
	Name string `json:"name"` // short file name
	Path string `json:"path"` // full path within the torrent (preserves folders)
	Size int64  `json:"size"`
	Done bool   `json:"done"`
}

// uiFilesResponse is the payload for the expandable per-file breakdown.
type uiFilesResponse struct {
	Files []uiFile `json:"files"`
}

// uiFiles returns one torrent's file list (by infohash) for the dashboard's
// expandable breakdown. Fetched lazily on expand so the main poll stays light.
func (h *Handler) uiFiles(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(r.URL.Query().Get("hash"))
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}
	t, ok, err := h.store.GetTorrent(r.Context(), hash)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	files, err := h.store.ListFiles(r.Context(), t.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	out := make([]uiFile, 0, len(files))
	for _, f := range files {
		name := f.ShortName
		if name == "" {
			name = f.RelPath
		}
		out = append(out, uiFile{Name: name, Path: f.RelPath, Size: f.Size, Done: f.Done})
	}
	h.writeJSON(w, r, uiFilesResponse{Files: out})
}

// uiPhase groups the internal state into a coarse phase the page colours by.
func uiPhase(state string) string {
	switch state {
	case store.StateQueued:
		return "queued"
	case store.StateTorBoxActive:
		return "torbox"
	case store.StateLocalQueued, store.StateLocalDload:
		return "local"
	case store.StateComplete:
		return "done"
	case store.StateError:
		return "error"
	default:
		return "queued"
	}
}

// uiStateLabel is a human-readable status line for the dashboard.
func uiStateLabel(t store.Torrent) string {
	switch t.State {
	case store.StateQueued:
		return "Queued for TorBox slot"
	case store.StateTorBoxActive:
		if t.TorBoxProgress > 0 {
			return "TorBox caching"
		}
		return "Waiting on TorBox"
	case store.StateLocalQueued:
		return "Ready, waiting to download"
	case store.StateLocalDload:
		return "Downloading to disk"
	case store.StateComplete:
		return "Complete"
	case store.StateError:
		return "Error"
	default:
		return t.State
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
