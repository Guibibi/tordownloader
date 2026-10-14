package qbit

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/guibibi/tordownloader/internal/downloader"
	"github.com/guibibi/tordownloader/internal/metrics"
	"github.com/guibibi/tordownloader/internal/store"
)

// SetMetricsInfo supplies the static values /metrics reports alongside live
// state: the build version and the configured TorBox slot limit.
func (h *Handler) SetMetricsInfo(version string, maxSlots int) {
	h.version = version
	h.maxSlots = maxSlots
}

// metricsHandler serves Prometheus metrics: point-in-time gauges read from the
// store and the download filesystem at scrape time, followed by the process-wide
// counters the engine and TorBox client maintain. Rendered into a buffer first
// so a store error yields a clean 500 instead of a truncated scrape.
func (h *Handler) metricsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	states, err := h.store.CountStates(ctx)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	torrents, err := h.store.ListTorrents(ctx, store.TorrentFilter{})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	// dlspeed means TorBox's fetch speed while TORBOX_ACTIVE and our CDN pull
	// speed while LOCAL_DOWNLOAD, so report them as separate stages.
	var torboxSpeed, localSpeed float64
	for _, t := range torrents {
		switch t.State {
		case store.StateTorBoxActive:
			torboxSpeed += float64(t.DLSpeed)
		case store.StateLocalDload:
			localSpeed += float64(t.DLSpeed)
		}
	}

	var buf bytes.Buffer
	mw := metrics.NewWriter(&buf)
	version := h.version
	if version == "" {
		version = "unknown"
	}
	mw.GaugeVec("tordownloader_build_info", "Build information; always 1.", "version",
		map[string]float64{version: 1})
	stateSeries := make(map[string]float64, len(states))
	for s, n := range states {
		stateSeries[s] = float64(n)
	}
	mw.GaugeVec("tordownloader_torrents", "Tracked torrents by internal state.", "state", stateSeries)
	mw.Gauge("tordownloader_torbox_slots_used",
		"TorBox slots in use (torrents TorBox is actively fetching).", float64(states[store.StateTorBoxActive]))
	if h.maxSlots > 0 {
		mw.Gauge("tordownloader_torbox_slots_max", "Configured TorBox slot limit (torbox.max_active_slots).",
			float64(h.maxSlots))
	}
	mw.GaugeVec("tordownloader_download_speed_bytes",
		"Current download speed in bytes/s, by stage (torbox = TorBox fetching, local = CDN to disk).", "stage",
		map[string]float64{"torbox": torboxSpeed, "local": localSpeed})
	if free, ok := downloader.FreeSpace(h.root); ok {
		mw.Gauge("tordownloader_disk_free_bytes", "Free bytes on the download root's filesystem.", float64(free))
	}
	mw.WriteCounters()
	if err := mw.Err(); err != nil {
		h.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
