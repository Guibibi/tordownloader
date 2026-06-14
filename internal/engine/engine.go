// Package engine orchestrates the torrent lifecycle: it submits accepted
// torrents to TorBox under the plan's slot limit, and (in later milestones)
// reconciles state and drives local downloads.
//
// M3 implements the submission gate: a worker pops QUEUED torrents and calls
// createtorrent while the number of TORBOX_ACTIVE torrents is below
// max_active_slots (3 on the Essential plan). Rate limits (429) leave a torrent
// QUEUED to retry; hard TorBox rejections (e.g. >200GB) move it to ERROR.
package engine

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// submitInterval is how often the submitter checks for QUEUED work. It only
// calls createtorrent when there is both a queued torrent and a free slot, so a
// short cadence just lowers add-to-submit latency without risking the rate limit.
const submitInterval = 2 * time.Second

// downloadInterval is how often the downloader looks for torrents whose content
// is present on TorBox and ready to pull to disk.
const downloadInterval = 2 * time.Second

// Defaults applied when Config leaves a field unset.
const (
	defaultPollInterval     = 10 * time.Second
	defaultFailTimeout      = 20 * time.Minute
	defaultParallelFiles    = 4
	defaultIncompleteSubdir = ".incomplete"
)

// TorBoxAPI is the slice of the TorBox client the engine needs: submitting
// releases (submitter), polling account state (reconciler), requesting CDN
// links (downloader), and deleting torrents (cleanup). The concrete
// *torbox.Client satisfies it; tests substitute a fake.
type TorBoxAPI interface {
	CreateTorrent(ctx context.Context, r torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error)
	MyList(ctx context.Context, bypassCache bool) ([]torbox.Torrent, error)
	RequestDL(ctx context.Context, p torbox.RequestDLParams) (string, error)
	ControlTorrent(ctx context.Context, torrentID int, op torbox.Operation) error
	CheckCached(ctx context.Context, hashes []string, listFiles bool) (map[string]torbox.CachedInfo, error)
}

// Config tunes the engine's background workers.
type Config struct {
	MaxSlots         int           // TorBox concurrent-slot limit (default 1)
	PollInterval     time.Duration // reconciler cadence (default 10s)
	FailTimeout      time.Duration // fail-fast window while TORBOX_ACTIVE (default 20m)
	ParallelFiles    int           // concurrent file downloads per torrent (default 4)
	IncompleteSubdir string        // staging dir under the save path (default .incomplete)
	CacheCheck       bool          // log TorBox cache status on submit (informational)
}

// activeDownload tracks the currently in-flight download so a delete can
// cancel it before cleaning up local files.
type activeDownload struct {
	cancel    context.CancelFunc
	infohash  string
	torrentID int64
}

// Engine runs the background workers.
type Engine struct {
	store      *store.Store
	torbox     TorBoxAPI
	maxSlots   int
	pollEvery  time.Duration
	failAfter  time.Duration
	parallel   int
	incomplete string
	cacheCheck bool
	httpClient *http.Client
	log        *slog.Logger

	mu             sync.Mutex
	activeDownload *activeDownload
}

// New builds an Engine from cfg, clamping unset/invalid fields to defaults.
func New(st *store.Store, tb TorBoxAPI, cfg Config, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxSlots < 1 {
		cfg.MaxSlots = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.FailTimeout <= 0 {
		cfg.FailTimeout = defaultFailTimeout
	}
	if cfg.ParallelFiles < 1 {
		cfg.ParallelFiles = defaultParallelFiles
	}
	if cfg.IncompleteSubdir == "" {
		cfg.IncompleteSubdir = defaultIncompleteSubdir
	}
	return &Engine{
		store:      st,
		torbox:     tb,
		maxSlots:   cfg.MaxSlots,
		pollEvery:  cfg.PollInterval,
		failAfter:  cfg.FailTimeout,
		parallel:   cfg.ParallelFiles,
		incomplete: cfg.IncompleteSubdir,
		cacheCheck: cfg.CacheCheck,
		// No client timeout: downloads can be long; cancellation is via ctx.
		httpClient: &http.Client{},
		log:        log,
	}
}

// Run executes a one-time startup reconciliation to recover state from a
// previous crash, then drives the submitter, reconciler, and downloader until
// ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.log.Info("engine started",
		"max_active_slots", e.maxSlots, "poll_interval", e.pollEvery, "fail_timeout", e.failAfter,
		"parallel_files", e.parallel)

	// Rebuild in-memory state from SQLite + TorBox: re-sync active torrents
	// and recover any content already finalized on disk.
	if err := e.startupReconcile(ctx); err != nil {
		e.log.Error("startup reconciliation failed", "err", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); e.loop(ctx, "submit", submitInterval, e.submitPass) }()
	go func() { defer wg.Done(); e.loop(ctx, "reconcile", e.pollEvery, e.reconcilePass) }()
	go func() { defer wg.Done(); e.loop(ctx, "download", downloadInterval, e.downloadPass) }()
	wg.Wait()
	e.log.Info("engine stopped")
}

// loop runs pass on a ticker until ctx is cancelled, logging non-cancellation
// errors under name.
func (e *Engine) loop(ctx context.Context, name string, every time.Duration, pass func(context.Context) error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := pass(ctx); err != nil && ctx.Err() == nil {
				e.log.Error(name+" pass", "err", err)
			}
		}
	}
}

// submitPass submits as many QUEUED torrents as there are free TorBox slots.
func (e *Engine) submitPass(ctx context.Context) error {
	active, err := e.store.CountByState(ctx, store.StateTorBoxActive)
	if err != nil {
		return err
	}
	free := e.maxSlots - active
	if free <= 0 {
		return nil
	}
	queued, err := e.store.ListByState(ctx, store.StateQueued)
	if err != nil {
		return err
	}
	for _, t := range queued {
		if free <= 0 {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// submit returns true when the torrent left the QUEUED state (success or
		// hard error), i.e. it consumed (or attempted to consume) a slot.
		if e.submit(ctx, t) {
			free--
		}
	}
	return nil
}

// submit tries to push one torrent to TorBox. It returns true when the torrent
// is no longer QUEUED (became active, or errored). A transient failure (network
// or rate limit) returns false, leaving it QUEUED for a later pass.
func (e *Engine) submit(ctx context.Context, t store.Torrent) bool {
	magnet, blob, err := e.store.Source(ctx, t.ID)
	if err != nil {
		e.log.Error("read torrent source", "infohash", t.Infohash, "err", err)
		return false
	}

	req := torbox.CreateTorrentRequest{Name: t.Name}
	switch {
	case magnet != "":
		req.Magnet = magnet
	case len(blob) > 0:
		req.File = blob
		req.FileName = t.Infohash + ".torrent"
	default:
		e.fail(ctx, t, "no magnet or torrent file to submit")
		return true
	}

	// Informational cache check: log whether TorBox already has the content so a
	// long fetch isn't mistaken for a stall. Never gates submission — uncached
	// releases are still valid and the fail-fast clock (failAfter) covers ones
	// TorBox can't fetch in time.
	if e.cacheCheck {
		e.logCacheStatus(ctx, t.Infohash)
	}

	res, err := e.torbox.CreateTorrent(ctx, req)
	if err != nil {
		var apiErr *torbox.APIError
		if errors.As(err, &apiErr) {
			if apiErr.StatusCode == 429 {
				// Rate limited even after the client's own backoff: keep it
				// QUEUED and try again next pass.
				e.log.Warn("createtorrent rate limited; staying queued", "infohash", t.Infohash)
				return false
			}
			// Any other TorBox rejection (too large, bad magnet, ...) is terminal.
			e.fail(ctx, t, apiErr.Error())
			return true
		}
		// Network/transient error: retry next pass.
		e.log.Warn("createtorrent transient failure; staying queued", "infohash", t.Infohash, "err", err)
		return false
	}

	id := torboxID(res)
	if id == 0 {
		e.fail(ctx, t, "createtorrent returned no torrent id")
		return true
	}
	if err := e.store.MarkActive(ctx, t.ID, id); err != nil {
		e.log.Error("mark active", "infohash", t.Infohash, "err", err)
		return false
	}
	e.log.Info("submitted to TorBox", "infohash", t.Infohash, "torbox_id", id, "name", t.Name)
	return true
}

// logCacheStatus reports whether TorBox already has the content for infohash. It
// is purely informational (errors are swallowed) and never affects submission.
func (e *Engine) logCacheStatus(ctx context.Context, infohash string) {
	cached, err := e.torbox.CheckCached(ctx, []string{infohash}, false)
	if err != nil {
		e.log.Debug("cache check failed (continuing)", "infohash", infohash, "err", err)
		return
	}
	// Exactly one hash was queried, so any returned entry means it's cached
	// (avoids depending on the response's key casing).
	if len(cached) > 0 {
		e.log.Info("cached on TorBox; content should be available shortly", "infohash", infohash)
	} else {
		e.log.Info("not cached on TorBox; will fetch server-side within the fail-fast window",
			"infohash", infohash, "fail_timeout", e.failAfter)
	}
}

// fail moves a torrent to ERROR, logging if the store update itself fails.
func (e *Engine) fail(ctx context.Context, t store.Torrent, reason string) {
	e.log.Warn("torrent failed", "infohash", t.Infohash, "reason", reason)
	if err := e.store.MarkError(ctx, t.ID, reason); err != nil {
		e.log.Error("mark error", "infohash", t.Infohash, "err", err)
	}
}

// torboxID picks the operational id from a createtorrent result, preferring the
// active torrent id over a queued id.
func torboxID(res *torbox.CreateTorrentResult) int {
	switch {
	case res == nil:
		return 0
	case res.TorrentID != nil:
		return *res.TorrentID
	case res.QueuedID != nil:
		return *res.QueuedID
	default:
		return 0
	}
}

// DeleteFunc is the signature the qbit layer calls to remove a torrent:
// cancels any in-flight download, tells TorBox to delete, removes local files,
// and drops the database row.
type DeleteFunc func(ctx context.Context, infohash string, deleteFiles bool) error

// DeleteTorrent removes a torrent end-to-end: cancels a running local download,
// calls controltorrent delete on TorBox (best-effort), removes local files when
// deleteFiles is true, and drops the database row.
func (e *Engine) DeleteTorrent(ctx context.Context, infohash string, deleteFiles bool) error {
	// 1. Cancel the in-flight download if this torrent is currently being pulled.
	e.mu.Lock()
	if e.activeDownload != nil && e.activeDownload.infohash == infohash {
		e.activeDownload.cancel()
	}
	e.mu.Unlock()

	// Detach the cleanup from the caller's request context. Once Sonarr asks to
	// delete, the TorBox removal, local file cleanup, and DB drop must all run to
	// completion even if the HTTP request is cancelled mid-flight — otherwise we
	// could remove the files (os.RemoveAll ignores ctx) but leave the row, which
	// would still show in torrents/info. Bound it so a hung TorBox call can't
	// block forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// 2. Fetch the row to know the TorBox id and local paths.
	t, ok, err := e.store.GetTorrent(ctx, infohash)
	if err != nil {
		return err
	}
	if !ok {
		return nil // already gone (or never tracked)
	}

	// 3. Delete from TorBox (best-effort: if the call fails we still clean up
	//    locally — the torrent will naturally expire on TorBox).
	if t.TorBoxID.Valid {
		if err := e.torbox.ControlTorrent(ctx, int(t.TorBoxID.Int64), torbox.OpDelete); err != nil {
			e.log.Warn("TorBox delete failed (continuing with local cleanup)",
				"infohash", infohash, "torbox_id", t.TorBoxID.Int64, "err", err)
		}
	}

	// 4. Remove local files when the caller requested it (Sonarr passes
	//    deleteFiles=true after importing, keeping /downloads clean).
	if deleteFiles {
		// Staging tree: <save_path>/.incomplete/<infohash>/...
		if t.SavePath != "" {
			staging := filepath.Join(t.SavePath, e.incomplete, t.Infohash)
			if err := os.RemoveAll(staging); err != nil && !os.IsNotExist(err) {
				e.log.Warn("remove staging", "path", staging, "err", err)
			}
		}

		// Downloaded content: the content_path (folder or single file).
		if t.ContentPath != "" && t.ContentPath != t.SavePath {
			if err := os.RemoveAll(t.ContentPath); err != nil && !os.IsNotExist(err) {
				e.log.Warn("remove content", "path", t.ContentPath, "err", err)
			}
		}

		// Also remove the incomplete parent dir if now empty (best-effort).
		if t.SavePath != "" {
			_ = os.Remove(filepath.Join(t.SavePath, e.incomplete)) // succeeds only if empty
		}
	}

	// 5. Drop the database row (files cascade via FK).
	if _, err := e.store.DeleteByHashes(ctx, []string{infohash}); err != nil {
		return err
	}
	e.log.Info("torrent deleted", "infohash", infohash, "delete_files", deleteFiles)
	return nil
}
