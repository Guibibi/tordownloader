// Package qbit emulates the qBittorrent WebUI API v2 so Sonarr/Radarr can use
// tordownloader as a download client. This file wires the routes; the handlers
// live alongside it. See docs/API_REFERENCE.md §1–§3 for the contract.
//
// M2 covers the read/identity surface: auth, app info/preferences, the
// read-only torrent endpoints, categories, and transfer info. Write endpoints
// (add/delete/pause/...) arrive in later milestones.
package qbit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/guibibi/tordownloader/internal/engine"
	"github.com/guibibi/tordownloader/internal/store"
)

// qBittorrent version strings we report. rdt-client reports the same pair, which
// Sonarr/Radarr accept; we match it for compatibility (docs/API_REFERENCE.md §1).
const (
	appVersion    = "v4.3.2"
	webAPIVersion = "2.7"
)

// Handler implements the qBittorrent WebUI API. It reads tracked state from the
// store and renders it as qBittorrent JSON.
type Handler struct {
	store      *store.Store
	root       string // download root; the qBittorrent "save_path" and default category base
	httpClient *http.Client
	log        *slog.Logger

	// failure mirrors the engine's failure policy so the dashboard can show, per
	// torrent, how much grace it has left and what speed floor it is held to. The
	// handler never enforces any of it — the engine does — it only reports it.
	failure FailurePolicy

	// deleteFn is called to remove a torrent end-to-end: TorBox deletion,
	// local file cleanup, and DB drop. When nil, the handler falls back to
	// store.DeleteByHashes (DB-only removal, for testing).
	deleteFn engine.DeleteFunc
}

// FailurePolicy mirrors the engine's failure.* settings for display purposes.
// It is passed as a struct rather than as positional arguments because the
// policy has grown several near-identical durations that are trivial to
// transpose at a call site.
type FailurePolicy struct {
	// StallTimeout is failure.stall_timeout: the grace a fetch stalled at 0%
	// gets. 0 means stall-failing is disabled for that tier and no countdown is
	// shown.
	StallTimeout time.Duration
	// ProgressStallTimeout is failure.progress_stall_timeout: the longer grace a
	// fetch that has made real progress (>0%) gets. 0 disables it for them.
	ProgressStallTimeout time.Duration
	// CachedStallTimeout is failure.cached_stall_timeout: the grace a cached
	// release gets while TorBox surfaces it. 0 disables it for them.
	CachedStallTimeout time.Duration
	// MinSpeed is failure.min_speed in bytes/s: the average a moving fetch must
	// sustain over SlowWindow. 0 means the speed check is off.
	MinSpeed int64
	// SlowWindow is failure.slow_window: the averaging window for MinSpeed.
	SlowWindow time.Duration
}

// New builds a Handler. root is the download root reported in app/preferences
// and used as the base for category save paths. failure mirrors the engine's
// failure.* policy so the dashboard can show the right countdown and speed floor
// per torrent. deleteFn is the engine's delete function; when nil the handler
// falls back to store-level DB-only deletion (useful in tests).
func New(st *store.Store, root string, failure FailurePolicy, deleteFn engine.DeleteFunc, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		store:      st,
		root:       root,
		failure:    failure,
		deleteFn:   deleteFn,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		log:        log,
	}
}

// Routes returns the HTTP handler for the emulated API, rooted at /api/v2.
// Endpoints accept both GET and POST as qBittorrent does, so no method is
// pinned except where it matters.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Auth & app.
	mux.HandleFunc("/api/v2/auth/login", h.login)
	mux.HandleFunc("/api/v2/auth/logout", h.logout)
	mux.HandleFunc("/api/v2/app/version", h.appVersion)
	mux.HandleFunc("/api/v2/app/webapiVersion", h.appWebAPIVersion)
	mux.HandleFunc("/api/v2/app/buildInfo", h.appBuildInfo)
	mux.HandleFunc("/api/v2/app/preferences", h.appPreferences)
	mux.HandleFunc("/api/v2/app/setPreferences", h.ok200)

	// Torrents — read.
	mux.HandleFunc("/api/v2/torrents/info", h.torrentsInfo)
	mux.HandleFunc("/api/v2/torrents/properties", h.torrentsProperties)
	mux.HandleFunc("/api/v2/torrents/files", h.torrentsFiles)

	// Torrents — write.
	mux.HandleFunc("/api/v2/torrents/add", h.torrentsAdd)
	mux.HandleFunc("/api/v2/torrents/delete", h.torrentsDelete)
	// No real pause concept on debrid; accept and 200 so Sonarr stays happy.
	mux.HandleFunc("/api/v2/torrents/pause", h.ok200)
	mux.HandleFunc("/api/v2/torrents/resume", h.ok200)
	mux.HandleFunc("/api/v2/torrents/setForceStart", h.ok200)
	mux.HandleFunc("/api/v2/torrents/topPrio", h.ok200)
	mux.HandleFunc("/api/v2/torrents/setShareLimits", h.ok200)
	mux.HandleFunc("/api/v2/torrents/recheck", h.ok200)
	mux.HandleFunc("/api/v2/torrents/reannounce", h.ok200)

	// Categories.
	mux.HandleFunc("/api/v2/torrents/categories", h.torrentsCategories)
	mux.HandleFunc("/api/v2/torrents/createCategory", h.createCategory)
	mux.HandleFunc("/api/v2/torrents/setCategory", h.setCategory)
	mux.HandleFunc("/api/v2/torrents/removeCategories", h.removeCategories)

	// Transfer.
	mux.HandleFunc("/api/v2/transfer/info", h.transferInfo)

	// Liveness for Docker/Unraid healthchecks. Unlike app/version (a static
	// string), this exercises the SQLite store, so a wedged database turns the
	// container unhealthy.
	mux.HandleFunc("/healthz", h.healthz)

	// Status dashboard (simple read-only UI). "/" also acts as the catch-all,
	// so uiIndex 404s anything that isn't exactly "/".
	mux.HandleFunc("/", h.uiIndex)
	mux.HandleFunc("/ui/torrents", h.uiTorrents)
	mux.HandleFunc("/ui/torrents/files", h.uiFiles)

	return mux
}

// writeJSON encodes v as JSON. On encode failure it logs and emits a 500.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.log.Error("encode response", "path", r.URL.Path, "err", err)
	}
}

// writeText writes a plain-text body (qBittorrent returns "Ok." for several
// endpoints).
func (h *Handler) writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	_, _ = w.Write([]byte(body))
}

// serverError logs err and returns a 500. Used when the store fails.
func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("handler error", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// ok200 is a stub handler that returns an empty 200, used for endpoints Sonarr
// calls but where we have nothing to do (e.g. setPreferences).
func (h *Handler) ok200(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
