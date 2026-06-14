package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// vanishGrace is how long after submission we tolerate a torrent being absent
// from mylist before declaring it vanished. createtorrent returns an id that
// should appear in a bypass_cache mylist immediately, but this absorbs brief
// propagation lag so a freshly-submitted torrent isn't failed by a race.
const vanishGrace = 30 * time.Second

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
	byID := make(map[int]torbox.Torrent, len(list))
	for _, t := range list {
		byID[t.ID] = t
	}
	for _, t := range active {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.reconcileOne(ctx, t, byID)
	}
	return nil
}

// reconcileOne advances a single TORBOX_ACTIVE torrent against its mylist entry.
func (e *Engine) reconcileOne(ctx context.Context, t store.Torrent, byID map[int]torbox.Torrent) {
	tb, ok := byID[int(t.TorBoxID.Int64)]
	if !t.TorBoxID.Valid || !ok {
		// Absent from a bypass_cache mylist means it genuinely vanished (removed
		// or expired). Tolerate the brief window right after submission.
		if t.ActiveSince > 0 && time.Since(time.Unix(t.ActiveSince, 0)) < vanishGrace {
			return
		}
		e.fail(ctx, t, "torrent vanished from TorBox")
		return
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

	// Still fetching: refresh live stats so Sonarr sees progress move.
	if err := e.store.UpdateTorBoxStatus(ctx, t.ID, store.TorBoxStatus{
		State:    tb.DownloadState,
		Progress: clamp01(tb.Progress),
		DLSpeed:  tb.DownloadSpeed,
		Size:     tb.Size,
		Name:     firstNonEmpty(t.Name, tb.Name),
	}); err != nil {
		e.log.Error("update torbox status", "infohash", t.Infohash, "err", err)
		return
	}

	if t.ActiveSince == 0 {
		return
	}
	activeFor := time.Since(time.Unix(t.ActiveSince, 0))

	// Early fast-fail: TorBox itself reports the torrent has no seeders and has
	// fetched nothing — a dead/unseeded release the full window won't rescue.
	// Failing now lets Sonarr/Radarr fail over to a better-seeded release in
	// minutes instead of 20. Torrents with any seed/progress keep the full window.
	if e.stallAfter > 0 && activeFor > e.stallAfter && isDeadStall(tb) {
		e.fail(ctx, t, fmt.Sprintf("stalled with no seeders for %s (download_state=%q, peers=%d)",
			e.stallAfter, tb.DownloadState, tb.Peers))
		return
	}

	// Fail-fast: the clock runs from active_since (set when it became active).
	if activeFor > e.failAfter {
		e.fail(ctx, t, fmt.Sprintf("not available within %s of becoming active", e.failAfter))
	}
}

// isDeadStall reports whether TorBox shows a torrent it cannot make progress on:
// no seeders, nothing downloaded, no transfer, and not served from cache. These
// hard signals (rather than the download_state string, whose exact wording
// varies) keep the check robust — a torrent that finds even one seeder or moves
// any bytes is excluded and keeps the full fail-fast window.
func isDeadStall(tb torbox.Torrent) bool {
	return tb.Seeds == 0 &&
		tb.Progress == 0 &&
		tb.DownloadSpeed == 0 &&
		!tb.Cached &&
		!tb.DownloadPresent
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
