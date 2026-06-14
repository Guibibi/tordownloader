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
	"sync"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// submitInterval is how often the submitter checks for QUEUED work. It only
// calls createtorrent when there is both a queued torrent and a free slot, so a
// short cadence just lowers add-to-submit latency without risking the rate limit.
const submitInterval = 2 * time.Second

// Defaults applied when Config leaves a field unset.
const (
	defaultPollInterval = 10 * time.Second
	defaultFailTimeout  = 20 * time.Minute
)

// TorBoxAPI is the slice of the TorBox client the engine needs: submitting
// releases (submitter) and polling account state (reconciler). The concrete
// *torbox.Client satisfies it; tests substitute a fake.
type TorBoxAPI interface {
	CreateTorrent(ctx context.Context, r torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error)
	MyList(ctx context.Context, bypassCache bool) ([]torbox.Torrent, error)
}

// Config tunes the engine's background workers.
type Config struct {
	MaxSlots     int           // TorBox concurrent-slot limit (default 1)
	PollInterval time.Duration // reconciler cadence (default 10s)
	FailTimeout  time.Duration // fail-fast window while TORBOX_ACTIVE (default 20m)
}

// Engine runs the background workers.
type Engine struct {
	store     *store.Store
	torbox    TorBoxAPI
	maxSlots  int
	pollEvery time.Duration
	failAfter time.Duration
	log       *slog.Logger
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
	return &Engine{
		store:     st,
		torbox:    tb,
		maxSlots:  cfg.MaxSlots,
		pollEvery: cfg.PollInterval,
		failAfter: cfg.FailTimeout,
		log:       log,
	}
}

// Run drives the submitter and reconciler until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.log.Info("engine started",
		"max_active_slots", e.maxSlots, "poll_interval", e.pollEvery, "fail_timeout", e.failAfter)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.loop(ctx, "submit", submitInterval, e.submitPass) }()
	go func() { defer wg.Done(); e.loop(ctx, "reconcile", e.pollEvery, e.reconcilePass) }()
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
