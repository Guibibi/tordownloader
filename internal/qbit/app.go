package qbit

import (
	"context"
	"net/http"
	"time"
)

// login accepts any credentials (LAN trust, no real auth) and sets a SID cookie
// because Sonarr expects one. Body is "Ok." as the real WebUI returns.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "SID",
		Value:    "tordownloader",
		Path:     "/",
		HttpOnly: true,
	})
	h.writeText(w, "Ok.")
}

// logout clears the SID cookie and returns 200.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "SID",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) appVersion(w http.ResponseWriter, r *http.Request) {
	h.writeText(w, appVersion)
}

func (h *Handler) appWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	h.writeText(w, webAPIVersion)
}

// appBuildInfo returns a static build-info blob. qBittorrent reports library
// versions here; the values are not meaningful to Sonarr but the shape must be
// valid JSON.
func (h *Handler) appBuildInfo(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, r, map[string]any{
		"qt":         "5.15.2",
		"libtorrent": "1.2.11.0",
		"boost":      "1.71.0",
		"openssl":    "1.1.1f",
		"bitness":    64,
	})
}

// healthz answers 200 "ok" when the store can run a trivial query, 503
// otherwise. Bounded so a wedged database fails the check instead of hanging it.
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.store.Ping(ctx); err != nil {
		h.log.Error("healthz: store ping", "err", err)
		http.Error(w, "unhealthy: store unreachable", http.StatusServiceUnavailable)
		return
	}
	h.writeText(w, "ok")
}

// appPreferences returns the few preferences Sonarr reads. save_path is the real
// download root; the rest are sane static values. setPreferences is a no-op stub.
func (h *Handler) appPreferences(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, r, map[string]any{
		"save_path":                h.root,
		"temp_path":                h.root,
		"temp_path_enabled":        false,
		"create_subfolder_enabled": true,
		"queueing_enabled":         false,
		"dht":                      true,
		"max_active_downloads":     3,
		"max_active_torrents":      3,
		"max_active_uploads":       3,
		"max_ratio_enabled":        false,
		"max_ratio":                -1,
	})
}
