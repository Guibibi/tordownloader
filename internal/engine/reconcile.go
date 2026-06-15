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

// torboxQueued is the torbox_state we record for a torrent TorBox has queued
// (accepted but not yet working). It marks the torrent so the reconciler waits
// for it to activate instead of failing it as vanished/stalled.
const torboxQueued = "queued"

// reconcilePass polls TorBox for every TORBOX_ACTIVE torrent and advances it:
// refreshes progress, moves torrents whose content is present to LOCAL_QUEUED,
// and fails torrents that vanish or exceed the fail-fast timeout.
func (e *Engine) reconcilePass(ctx context.Context) error {
	active, err := e.store.ListByState(ctx, store.StateTorBoxActive)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}
	// bypass_cache so a just-finished torrent is seen promptly (mylist is
	// otherwise cached ~10m, which would stall the hand-off to the downloader).
	list, err := e.torbox.MyList(ctx, true)
	if err != nil {
		// Transient: leave torrents active and retry next pass.
		return fmt.Errorf("mylist: %w", err)
	}
	// Index by both id and infohash. Hash matching lets us re-find a torrent that
	// TorBox re-activated under a new id (e.g. promoted from its queue).
	byID := make(map[int]torbox.Torrent, len(list))
	byHash := make(map[string]torbox.Torrent, len(list))
	for _, t := range list {
		byID[t.ID] = t
		if t.Hash != "" {
			byHash[strings.ToLower(t.Hash)] = t
		}
	}
	for _, t := range active {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.reconcileOne(ctx, t, byID, byHash)
	}
	return nil
}

// reconcileOne advances a single TORBOX_ACTIVE torrent against its mylist entry.
func (e *Engine) reconcileOne(ctx context.Context, t store.Torrent, byID map[int]torbox.Torrent, byHash map[string]torbox.Torrent) {
	tb, ok := lookupTorBox(t, byID, byHash)
	if !ok {
		// Not in mylist. A torrent we submitted into TorBox's queue won't appear
		// until it activates, so don't fail it while it's known-queued.
		if t.TorBoxState == torboxQueued {
			return
		}
		// Within the grace window, tolerate brief post-submit propagation lag.
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

	// Still waiting in TorBox's queue (accepted, not yet working): keep its clocks
	// pending (Advancing) and don't apply the stall/vanish checks. Once it starts,
	// the stall clock effectively runs from when it left the queue.
	if isQueuedOnTorBox(tb) {
		if err := e.store.UpdateTorBoxStatus(ctx, t.ID, store.TorBoxStatus{
			State: torboxQueued, Advancing: true,
		}); err != nil {
			e.log.Error("update torbox status", "infohash", t.Infohash, "err", err)
		}
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

	// Stall fail: the torrent is making no headway — no new bytes and progress
	// isn't climbing. If it's been stuck this long it's a dead/unseeded release
	// the wait won't rescue, so fail it: that reports an error to Sonarr/Radarr
	// which blacklists the release and grabs another. A slow but moving download
	// keeps resetting progress_at (via Advancing) and is never failed for speed.
	if e.stallAfter > 0 && !advancing {
		stalledSince := t.ProgressAt
		if stalledSince == 0 {
			stalledSince = t.ActiveSince
		}
		if now.Sub(time.Unix(stalledSince, 0)) > e.stallAfter {
			e.fail(ctx, t, fmt.Sprintf("no download progress for %s (download_state=%q, seeds=%d, peers=%d)",
				e.stallAfter, tb.DownloadState, tb.Seeds, tb.Peers))
			return
		}
	}

	// Optional absolute cap from active_since. Disabled by default
	// (failure.timeout = 0); set it to bound how long a perpetually-slow torrent
	// may hold a scarce TorBox slot.
	if e.failAfter > 0 && now.Sub(time.Unix(t.ActiveSince, 0)) > e.failAfter {
		e.fail(ctx, t, fmt.Sprintf("not available within %s of becoming active", e.failAfter))
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

// isQueuedOnTorBox reports whether TorBox has the torrent queued (accepted but
// not yet in an active download slot) rather than actively working it. Such a
// torrent must not be treated as stalled. `Active` is the primary signal; the
// extra guards mean anything actually moving (progress/bytes) or finished is
// never misread as queued, so the stall detector is preserved.
func isQueuedOnTorBox(tb torbox.Torrent) bool {
	return !tb.Active &&
		tb.Progress == 0 &&
		tb.DownloadSpeed == 0 &&
		!tb.DownloadPresent &&
		!tb.DownloadFinished
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
