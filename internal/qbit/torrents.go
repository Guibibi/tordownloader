package qbit

import (
	"net/http"
	"strings"

	"github.com/guibibi/tordownloader/internal/store"
)

// torrentsInfo returns the array of torrent objects Sonarr polls. Supports the
// `category` and `hashes` filters qBittorrent documents.
func (h *Handler) torrentsInfo(w http.ResponseWriter, r *http.Request) {
	f := store.TorrentFilter{}
	if r.URL.Query().Has("category") {
		cat := r.URL.Query().Get("category")
		f.Category = &cat
	}
	if hs := r.URL.Query().Get("hashes"); hs != "" && hs != "all" {
		f.Hashes = splitHashes(hs)
	}

	torrents, err := h.store.ListTorrents(r.Context(), f)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	// Always emit an array, never null, so clients see "[]" when empty.
	out := make([]torrentInfo, 0, len(torrents))
	for _, t := range torrents {
		out = append(out, renderTorrent(t))
	}
	h.writeJSON(w, r, out)
}

// torrentProperties is the per-hash detail object from torrents/properties.
type torrentProperties struct {
	SavePath        string  `json:"save_path"`
	TotalSize       int64   `json:"total_size"`
	TotalDownloaded int64   `json:"total_downloaded"`
	TotalUploaded   int64   `json:"total_uploaded"`
	AdditionDate    int64   `json:"addition_date"`
	CompletionDate  int64   `json:"completion_date"`
	DLSpeed         int64   `json:"dl_speed"`
	DLSpeedAvg      int64   `json:"dl_speed_avg"`
	UpSpeed         int64   `json:"up_speed"`
	ETA             int64   `json:"eta"`
	ShareRatio      float64 `json:"share_ratio"`
	Seeds           int     `json:"seeds"`
	Peers           int     `json:"peers"`
	Pieces          int     `json:"pieces_num"`
	PiecesHave      int     `json:"pieces_have"`
}

// torrentsProperties returns details for a single `hash`. 404 when not tracked.
func (h *Handler) torrentsProperties(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
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

	completed := int64(t.LocalProgress * float64(t.Size))
	if completed > t.Size {
		completed = t.Size
	}
	amountLeft := t.Size - completed
	eta := int64(etaUnknown)
	if t.DLSpeed > 0 && amountLeft > 0 {
		eta = amountLeft / t.DLSpeed
	}

	h.writeJSON(w, r, torrentProperties{
		SavePath:        t.SavePath,
		TotalSize:       t.Size,
		TotalDownloaded: completed,
		AdditionDate:    t.AddedOn,
		CompletionDate:  t.CompletedOn,
		DLSpeed:         t.DLSpeed,
		ETA:             eta,
		Seeds:           0,
		Peers:           0,
	})
}

// torrentFile is one entry in the torrents/files array.
type torrentFile struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	Priority     int     `json:"priority"`
	IsSeed       bool    `json:"is_seed"`
	Availability float64 `json:"availability"`
}

// torrentsFiles returns the per-file list for a single `hash`.
func (h *Handler) torrentsFiles(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
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

	out := make([]torrentFile, 0, len(files))
	for i, f := range files {
		var progress float64
		switch {
		case f.Done:
			progress = 1
		case f.Size > 0:
			progress = float64(f.Downloaded) / float64(f.Size)
		}
		out = append(out, torrentFile{
			Index:        i,
			Name:         f.RelPath,
			Size:         f.Size,
			Progress:     progress,
			Priority:     1,
			Availability: -1,
		})
	}
	h.writeJSON(w, r, out)
}

// torrentsCategories returns the configured categories keyed by name.
func (h *Handler) torrentsCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := h.store.ListCategories(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	out := make(map[string]map[string]string, len(cats))
	for _, c := range cats {
		out[c.Name] = map[string]string{"name": c.Name, "savePath": c.SavePath}
	}
	h.writeJSON(w, r, out)
}

// splitHashes parses qBittorrent's pipe-separated, lowercased hash list.
func splitHashes(s string) []string {
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}
