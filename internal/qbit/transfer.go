package qbit

import (
	"net/http"

	"github.com/guibibi/tordownloader/internal/store"
)

// transferInfoResp is the global transfer snapshot from transfer/info.
type transferInfoResp struct {
	ConnectionStatus string `json:"connection_status"`
	DLInfoSpeed      int64  `json:"dl_info_speed"`
	UpInfoSpeed      int64  `json:"up_info_speed"`
	DLInfoData       int64  `json:"dl_info_data"`
	UpInfoData       int64  `json:"up_info_data"`
	DLRateLimit      int64  `json:"dl_rate_limit"`
	UpRateLimit      int64  `json:"up_rate_limit"`
	DHTNodes         int    `json:"dht_nodes"`
}

// transferInfo reports "connected" so Sonarr's health check passes, plus the
// aggregate download speed summed across tracked torrents.
func (h *Handler) transferInfo(w http.ResponseWriter, r *http.Request) {
	torrents, err := h.store.ListTorrents(r.Context(), store.TorrentFilter{})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	var dl int64
	for _, t := range torrents {
		dl += t.DLSpeed
	}
	h.writeJSON(w, r, transferInfoResp{
		ConnectionStatus: "connected",
		DLInfoSpeed:      dl,
	})
}
