package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// vanishGrace is how long after submission we tolerate a torrent being absent
// from mylist before declaring it vanished. createtorrent returns an id that
// should appear in a bypass_cache mylist soon, but mylist can lag or page under
// load; this absorbs that, and past it we confirm with a direct lookup before
// failing (confirmPresent).
const vanishGrace = 2 * time.Minute

// reconcilePass polls TorBox once per cadence and does three things: reports
// unnotified failures back to Sonarr/Radarr (arrPass), reaps torrents that are
// finished with TorBox but still on the account (reapPass), and advances every
// TORBOX_ACTIVE torrent against a single mylist read (refresh progress, hand
// content off to the downloader, fail vanished/stalled ones).
func (e *Engine) reconcilePass(ctx context.Context) error {
	e.arrPass(ctx)

	active, err := e.store.ListByState(ctx, store.StateTorBoxActive)
	if err != nil {
		return err
	}
	e.pruneReannounce(active)
	reapable, err := e.store.ListReapable(ctx)
	if err != nil {
		return err
	}
	if len(active) == 0 && len(reapable) == 0 {
		return nil
	}
	// bypass_cache so a just-finished torrent is seen promptly (mylist is
	// otherwise cached ~10m, which would stall the hand-off to the downloader).
	list, err := e.torbox.MyList(ctx, true)
	if err != nil {
		// Transient: leave torrents active and retry next pass.
		return fmt.Errorf("mylist: %w", err)
	}
	byID, byHash := indexTorBox(list)

	// Free slots held by torrents already done with TorBox before advancing the
	// active ones, so a freed slot is available to the submitter next pass.
	e.reapPass(ctx, reapable, byID, byHash)

	for _, t := range active {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.reconcileOne(ctx, t, byID, byHash)
	}
	return nil
}

// indexTorBox keys a mylist by both torrent id and (lowercased) infohash. Hash
// matching lets us re-find a torrent TorBox re-activated under a new id.
func indexTorBox(list []torbox.Torrent) (map[int]torbox.Torrent, map[string]torbox.Torrent) {
	byID := make(map[int]torbox.Torrent, len(list))
	byHash := make(map[string]torbox.Torrent, len(list))
	for _, t := range list {
		byID[t.ID] = t
		if t.Hash != "" {
			byHash[strings.ToLower(t.Hash)] = t
		}
	}
	return byID, byHash
}

// reapPass deletes from TorBox every torrent that is finished with it (COMPLETE
// or ERROR) yet is still present on the account, then clears its refs. This is
// hygiene, not slot recovery: with seeding disabled a cached torrent is inactive
// and holds no slot, so a leftover — from a delete missed because the app was an
// older build, was restarted between completion and delete, or hit a transient
// TorBox error — only clutters mylist; it does not pin a slot or starve the queue.
// We still clean it up so the account's list stays tied to what we track.
func (e *Engine) reapPass(ctx context.Context, reapable []store.Torrent, byID map[int]torbox.Torrent, byHash map[string]torbox.Torrent) {
	if len(reapable) == 0 {
		return
	}
	e.log.Info("reaping leftover torrents on TorBox after completion/failure", "count", len(reapable))
	for _, t := range reapable {
		if ctx.Err() != nil {
			return
		}
		switch {
		case isPresent(t, byID, byHash):
			// Still on TorBox: delete it; removeFromTorBox clears refs on success.
			e.removeFromTorBox(ctx, t)
		case t.TorBoxID.Valid:
			// Had an active id but it's gone from mylist (e.g. deleted out-of-band):
			// nothing to delete, just free our accounting.
			if err := e.store.ClearTorBoxRefs(ctx, t.ID); err != nil {
				e.log.Warn("reap: clear refs (continuing)", "infohash", t.Infohash, "err", err)
			}
		case t.TorBoxQueuedID.Valid:
			// A queued entry never appears in mylist; best-effort delete + clear.
			e.removeFromTorBox(ctx, t)
		}
	}
}

// isPresent reports whether a torrent is still in the TorBox mylist (by id or
// infohash).
func isPresent(t store.Torrent, byID map[int]torbox.Torrent, byHash map[string]torbox.Torrent) bool {
	_, ok := lookupTorBox(t, byID, byHash)
	return ok
}

// reconcileOne advances a single TORBOX_ACTIVE torrent against its mylist entry.
func (e *Engine) reconcileOne(ctx context.Context, t store.Torrent, byID map[int]torbox.Torrent, byHash map[string]torbox.Torrent) {
	tb, ok := lookupTorBox(t, byID, byHash)
	if !ok {
		// Not in mylist. Within the grace window, tolerate brief post-submit
		// propagation lag.
		if t.ActiveSince > 0 && time.Since(time.Unix(t.ActiveSince, 0)) < vanishGrace {
			return
		}
		// Past grace: confirm with a direct lookup before failing, so a cache or
		// pagination gap in mylist doesn't kill a live torrent.
		if e.confirmPresent(ctx, t) {
			return
		}
		e.fail(ctx, t, "torrent vanished from TorBox")
		return
	}

	// Adopt a changed id (e.g. promoted from TorBox's queue under a new id) so all
	// later operations (requestdl, delete) use the live id.
	if tb.ID != 0 && (!t.TorBoxID.Valid || tb.ID != int(t.TorBoxID.Int64)) {
		e.log.Info("TorBox id changed; adopting",
			"infohash", t.Infohash, "old_id", t.TorBoxID.Int64, "new_id", tb.ID)
		if err := e.store.UpdateTorBoxID(ctx, t.ID, tb.ID); err != nil {
			e.log.Error("update torbox id", "infohash", t.Infohash, "err", err)
		}
	}

	// Content is available on TorBox: hand off to the local downloader.
	if tb.DownloadPresent {
		if len(tb.Files) == 0 {
			e.log.Warn("download_present but no files listed; waiting",
				"infohash", t.Infohash, "torbox_id", tb.ID)
			return
		}
		e.toLocalQueued(ctx, t, tb)
		return
	}

	// Still fetching: refresh live stats so Sonarr sees progress move. A torrent
	// is "advancing" when its progress climbs or TorBox is actively moving bytes;
	// that resets the stall clock so a slow-but-live download is never failed for
	// being slow.
	advancing := tb.Progress > t.TorBoxProgress || tb.DownloadSpeed > 0
	if err := e.store.UpdateTorBoxStatus(ctx, t.ID, store.TorBoxStatus{
		State:     tb.DownloadState,
		Progress:  clamp01(tb.Progress),
		DLSpeed:   tb.DownloadSpeed,
		Seeds:     tb.Seeds,
		Peers:     tb.Peers,
		ETA:       tb.ETA,
		Size:      tb.Size,
		Name:      firstNonEmpty(t.Name, tb.Name),
		Advancing: advancing,
	}); err != nil {
		e.log.Error("update torbox status", "infohash", t.Infohash, "err", err)
		return
	}

	if t.ActiveSince == 0 {
		return
	}
	now := time.Now()

	if advancing {
		// Moving again: forget the nudge bookkeeping so a future stall episode
		// starts a fresh reannounce cycle.
		if _, wasStalled := e.lastReannounce[t.ID]; wasStalled {
			e.log.Info("stalled torrent resumed progress",
				"infohash", t.Infohash, "progress", fmt.Sprintf("%.0f%%", clamp01(tb.Progress)*100))
			delete(e.lastReannounce, t.ID)
		}
	} else {
		// Stalled: no new bytes and progress isn't climbing. How long we tolerate
		// that depends on what's at stake (stallTimeoutFor): a 0% fetch is failed
		// quickly — abandoning it costs nothing and the resulting error makes
		// Sonarr/Radarr blacklist the release and grab another — while a fetch with
		// real progress gets hours, because thin swarms lose their seeds and recover,
		// and killing a partial download blacklists a release that would likely have
		// finished. A slow but moving download keeps resetting progress_at (via
		// Advancing) and is never failed for speed. While waiting, the stalled fetch
		// is periodically nudged with a reannounce (maybeReannounce) so a returning
		// seed is picked up promptly.
		stalledSince := t.ProgressAt
		if stalledSince == 0 {
			stalledSince = t.ActiveSince
		}
		stalledFor := now.Sub(time.Unix(stalledSince, 0))
		hasProgress := t.TorBoxProgress > 0 || tb.Progress > 0
		if stallAfter := e.stallTimeoutFor(t, hasProgress); stallAfter > 0 && stalledFor > stallAfter {
			e.fail(ctx, t, fmt.Sprintf("no download progress for %s (progress=%.0f%%, download_state=%q, seeds=%d, peers=%d, cached=%v)",
				stallAfter, clamp01(tb.Progress)*100, tb.DownloadState, tb.Seeds, tb.Peers, t.Cached))
			return
		}
		e.maybeReannounce(ctx, t, tb, stalledFor)
	}

	// Optional absolute cap from active_since. Disabled by default
	// (failure.timeout = 0); set it to bound how long a perpetually-slow torrent
	// may hold a scarce TorBox slot.
	if e.failAfter > 0 && now.Sub(time.Unix(t.ActiveSince, 0)) > e.failAfter {
		e.fail(ctx, t, fmt.Sprintf("not available within %s of becoming active", e.failAfter))
	}
}

// stallTimeoutFor returns the no-progress grace a torrent gets before it's
// failed. Three tiers: a release TorBox reported as cached waits on TorBox
// itself, not on peers (cachedStallAfter); a peer-fetch that has already made
// real progress gets the long progressStallAfter, because thin swarms drop to
// zero seeds and recover on a scale of hours and failing it would blacklist a
// release that would likely finish; a fetch that never left 0% gets the short
// stallAfter, since abandoning it costs nothing and lets Sonarr/Radarr fall back
// to another release sooner. A non-positive value disables the stall check for
// that tier.
func (e *Engine) stallTimeoutFor(t store.Torrent, hasProgress bool) time.Duration {
	switch {
	case t.Cached:
		return e.cachedStallAfter
	case hasProgress:
		return e.progressStallAfter
	default:
		return e.stallAfter
	}
}

// maybeReannounce nudges a stalled peer-fetch with a TorBox reannounce so its
// client re-contacts trackers, at most once per reannounceEvery. Stalls are
// often transient — seeds drop off and come back — and a reannounce picks a
// returning swarm up sooner than passively waiting on the stall clock. Cached
// releases are skipped (they wait on TorBox, not on peers). The nudge time is
// recorded per attempt (success or not) so a failing controltorrent call is not
// retried every poll tick.
func (e *Engine) maybeReannounce(ctx context.Context, t store.Torrent, tb torbox.Torrent, stalledFor time.Duration) {
	if e.reannounceEvery <= 0 || t.Cached || tb.ID == 0 {
		return
	}
	if stalledFor < e.reannounceEvery || time.Since(e.lastReannounce[t.ID]) < e.reannounceEvery {
		return
	}
	e.lastReannounce[t.ID] = time.Now()
	if err := e.torbox.ControlTorrent(ctx, tb.ID, torbox.OpReannounce); err != nil {
		e.log.Warn("reannounce stalled torrent (continuing)",
			"infohash", t.Infohash, "torbox_id", tb.ID, "err", err)
		return
	}
	e.log.Info("stalled; nudged TorBox to reannounce",
		"infohash", t.Infohash, "stalled_for", stalledFor.Round(time.Second),
		"progress", fmt.Sprintf("%.0f%%", clamp01(tb.Progress)*100),
		"seeds", tb.Seeds, "peers", tb.Peers)
}

// arrPass pushes each unreported ERROR torrent's failure to Sonarr/Radarr:
// the *arr that grabbed it removes its queue item, blocklists the release, and
// searches for a replacement — the failure loop qBittorrent state reporting
// alone can never close, because Sonarr maps qBit's "error" to a mere warning
// with failed-download handling deliberately not triggered. With
// removeFromClient=true the *arr then deletes the torrent from us too, so the
// ERROR row and any partial files clean themselves up.
//
// Per row: a notify error (instance unreachable) retries every
// arrNotifyInterval; a clean "no instance tracks this hash" retries until
// arrNotFoundGrace has passed since the first attempt (a near-instant failure
// can beat the *arr's queue refresh), then the row is marked notified so we
// stop asking — it stays visible as ERROR until deleted. Runs on the reconcile
// goroutine only.
func (e *Engine) arrPass(ctx context.Context) {
	if e.arr == nil {
		return
	}
	pending, err := e.store.ListArrUnnotified(ctx)
	if err != nil {
		e.log.Error("list unnotified failures", "err", err)
		return
	}
	now := time.Now()
	e.pruneArrAttempts(pending)
	for _, t := range pending {
		if ctx.Err() != nil {
			return
		}
		at, tried := e.arrAttempts[t.ID]
		if tried && now.Sub(at.last) < arrNotifyInterval {
			continue
		}
		if !tried {
			at.first = now
		}
		at.last = now
		e.arrAttempts[t.ID] = at

		handled, nerr := e.arr.NotifyFailed(ctx, t.Infohash, t.Name)
		switch {
		case handled:
			// Blocklisted + replacement search triggered; the *arr's follow-up
			// delete will clear the row.
			if err := e.store.MarkArrNotified(ctx, t.ID); err != nil {
				e.log.Error("mark arr notified", "infohash", t.Infohash, "err", err)
			}
			delete(e.arrAttempts, t.ID)
		case nerr != nil:
			// Some instance couldn't be checked; NotifyFailed already logged the
			// detail. Retry next interval.
		case now.Sub(at.first) >= arrNotFoundGrace:
			// Every instance answered and none tracks this hash — not an *arr grab
			// (or its queue item is already gone). Stop asking.
			e.log.Info("failed torrent not tracked by any *arr; leaving as ERROR",
				"infohash", t.Infohash, "name", t.Name)
			if err := e.store.MarkArrNotified(ctx, t.ID); err != nil {
				e.log.Error("mark arr notified", "infohash", t.Infohash, "err", err)
			}
			delete(e.arrAttempts, t.ID)
		}
	}
}

// pruneArrAttempts drops notify bookkeeping for rows no longer pending (marked
// notified, or deleted), mirroring pruneReannounce. Reconcile goroutine only.
func (e *Engine) pruneArrAttempts(pending []store.Torrent) {
	if len(e.arrAttempts) == 0 {
		return
	}
	keep := make(map[int64]bool, len(pending))
	for _, t := range pending {
		keep[t.ID] = true
	}
	for id := range e.arrAttempts {
		if !keep[id] {
			delete(e.arrAttempts, id)
		}
	}
}

// pruneReannounce drops reannounce bookkeeping for torrents that are no longer
// TORBOX_ACTIVE (failed, completed, or deleted) so the map cannot grow
// unbounded. Like lastReannounce itself, this runs only on the reconcile
// goroutine.
func (e *Engine) pruneReannounce(active []store.Torrent) {
	if len(e.lastReannounce) == 0 {
		return
	}
	keep := make(map[int64]bool, len(active))
	for _, t := range active {
		keep[t.ID] = true
	}
	for id := range e.lastReannounce {
		if !keep[id] {
			delete(e.lastReannounce, id)
		}
	}
}

// lookupTorBox finds a torrent's mylist entry, preferring its stored id and
// falling back to its infohash (so a re-activated torrent under a new id is still
// matched).
func lookupTorBox(t store.Torrent, byID map[int]torbox.Torrent, byHash map[string]torbox.Torrent) (torbox.Torrent, bool) {
	if t.TorBoxID.Valid {
		if tb, ok := byID[int(t.TorBoxID.Int64)]; ok {
			return tb, true
		}
	}
	if t.Infohash != "" {
		if tb, ok := byHash[strings.ToLower(t.Infohash)]; ok {
			return tb, true
		}
	}
	return torbox.Torrent{}, false
}

// confirmPresent does a direct id lookup to check a torrent really is gone before
// failing it as vanished. A lookup error (transient/rate limit) is treated as
// "present" so a flaky call never costs us a torrent; only a clean "not found"
// (no error, empty result) confirms it vanished.
func (e *Engine) confirmPresent(ctx context.Context, t store.Torrent) bool {
	if !t.TorBoxID.Valid {
		return false
	}
	tb, err := e.torbox.GetTorrent(ctx, int(t.TorBoxID.Int64), true)
	if err != nil {
		e.log.Warn("vanish confirm lookup failed; deferring", "infohash", t.Infohash, "err", err)
		return true
	}
	return tb != nil && tb.ID != 0
}

// toLocalQueued records the TorBox file list and moves the torrent to
// LOCAL_QUEUED for the downloader to pick up.
func (e *Engine) toLocalQueued(ctx context.Context, t store.Torrent, tb torbox.Torrent) {
	files := make([]store.FileInput, 0, len(tb.Files))
	for _, f := range tb.Files {
		files = append(files, store.FileInput{
			TorBoxFileID: f.ID,
			RelPath:      firstNonEmpty(f.Name, f.ShortName),
			ShortName:    f.ShortName,
			Size:         f.Size,
		})
	}
	name := firstNonEmpty(t.Name, tb.Name)
	size := t.Size
	if size == 0 {
		size = tb.Size
	}
	cp := contentPath(t.SavePath, name, files)
	if err := e.store.MarkLocalQueued(ctx, t.ID, cp, size, files); err != nil {
		e.log.Error("mark local queued", "infohash", t.Infohash, "err", err)
		return
	}
	e.log.Info("TorBox content present; queued for local download",
		"infohash", t.Infohash, "torbox_id", tb.ID, "files", len(files))
}

// contentPath computes the qBittorrent content_path: the file itself for a
// single-file torrent, otherwise the torrent's folder under the save path
// (docs/ARCHITECTURE.md §6).
func contentPath(savePath, name string, files []store.FileInput) string {
	if len(files) == 1 {
		return filepath.Join(savePath, files[0].RelPath)
	}
	if name != "" {
		return filepath.Join(savePath, name)
	}
	return savePath
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}
