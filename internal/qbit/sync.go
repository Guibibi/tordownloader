package qbit

import (
	"net/http"
	"sync/atomic"

	"github.com/guibibi/tordownloader/internal/downloader"
	"github.com/guibibi/tordownloader/internal/store"
)

// The endpoints in this file exist for remote-control apps (nzb360, qBitControl,
// the qBittorrent WebUI itself ...), not for Sonarr/Radarr, which only poll
// torrents/info. Those apps build their torrent list from sync/maindata; without
// it they connect fine (auth/login + app/version succeed) but show no downloads.

// syncRID is the response id handed out by sync/maindata. qBittorrent uses it to
// send deltas; we always send a full snapshot (full_update: true), which clients
// must accept for any rid, so it only has to keep increasing.
var syncRID atomic.Int64

// serverState is the server_state object in sync/maindata: the transfer/info
// fields plus the extras qBittorrent adds there.
type serverState struct {
	transferInfoResp
	FreeSpaceOnDisk   uint64 `json:"free_space_on_disk"`
	AllTimeDL         int64  `json:"alltime_dl"`
	AllTimeUL         int64  `json:"alltime_ul"`
	GlobalRatio       string `json:"global_ratio"`
	Queueing          bool   `json:"queueing"`
	UseAltSpeedLimits bool   `json:"use_alt_speed_limits"`
	RefreshInterval   int    `json:"refresh_interval"`
	TotalPeerConns    int    `json:"total_peer_connections"`
	AverageTimeQueue  int    `json:"average_time_queue"`
	ReadCacheHits     string `json:"read_cache_hits"`
	WriteCacheOver    string `json:"write_cache_overload"`
	ReadCacheOver     string `json:"read_cache_overload"`
	QueuedIOJobs      int    `json:"queued_io_jobs"`
	TotalBuffersSize  int64  `json:"total_buffers_size"`
	TotalQueuedSize   int64  `json:"total_queued_size"`
	TotalWastedSess   int64  `json:"total_wasted_session"`
}

// mainData is the sync/maindata response. Every map/slice is non-nil so clients
// see {} / [] rather than null.
type mainData struct {
	RID         int64                        `json:"rid"`
	FullUpdate  bool                         `json:"full_update"`
	Torrents    map[string]torrentInfo       `json:"torrents"`
	Removed     []string                     `json:"torrents_removed"`
	Categories  map[string]map[string]string `json:"categories"`
	CatsRemoved []string                     `json:"categories_removed"`
	Tags        []string                     `json:"tags"`
	TagsRemoved []string                     `json:"tags_removed"`
	Trackers    map[string][]string          `json:"trackers"`
	ServerState serverState                  `json:"server_state"`
}

// syncMainData returns a full snapshot of torrents, categories and server state.
// The `rid` parameter is accepted and ignored: a full update is always valid.
func (h *Handler) syncMainData(w http.ResponseWriter, r *http.Request) {
	torrents, err := h.store.ListTorrents(r.Context(), store.TorrentFilter{})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	cats, err := h.store.ListCategories(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	out := mainData{
		RID:         syncRID.Add(1),
		FullUpdate:  true,
		Torrents:    make(map[string]torrentInfo, len(torrents)),
		Removed:     []string{},
		Categories:  make(map[string]map[string]string, len(cats)),
		CatsRemoved: []string{},
		Tags:        []string{},
		TagsRemoved: []string{},
		Trackers:    map[string][]string{},
	}
	var dl int64
	for _, t := range torrents {
		out.Torrents[t.Infohash] = renderTorrent(t)
		dl += t.DLSpeed
	}
	for _, c := range cats {
		out.Categories[c.Name] = map[string]string{"name": c.Name, "savePath": c.SavePath}
	}
	free, _ := downloader.FreeSpace(h.root)
	out.ServerState = serverState{
		transferInfoResp: transferInfoResp{ConnectionStatus: "connected", DLInfoSpeed: dl},
		FreeSpaceOnDisk:  free,
		GlobalRatio:      "0.00",
		RefreshInterval:  1500,
		ReadCacheHits:    "0",
		WriteCacheOver:   "0",
		ReadCacheOver:    "0",
	}
	h.writeJSON(w, r, out)
}

// syncTorrentPeers answers the per-torrent peer list. We have no peers of our
// own (TorBox does the swarm work), so it is always empty.
func (h *Handler) syncTorrentPeers(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, r, map[string]any{
		"rid":         syncRID.Add(1),
		"full_update": true,
		"peers":       map[string]any{},
		"show_flags":  false,
	})
}

// emptyArray answers endpoints whose honest answer is "none" (trackers,
// webseeds, tags, piece states) with [] so detail views render instead of
// erroring on a 404.
func (h *Handler) emptyArray(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, r, []any{})
}

// speedLimitsMode reports alternative speed limits as off ("0"); we don't
// throttle, so toggleSpeedLimitsMode is a no-op stub.
func (h *Handler) speedLimitsMode(w http.ResponseWriter, _ *http.Request) {
	h.writeText(w, "0")
}
