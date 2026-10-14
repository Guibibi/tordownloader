package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"github.com/guibibi/tordownloader/internal/downloader"
	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// downloadPass launches downloads for every torrent whose content is present
// on TorBox, up to parallelTorrents at once. In-flight downloads
// (LOCAL_DOWNLOAD) get their slots before newly ready ones (LOCAL_QUEUED), so
// resumes aren't starved. Each torrent downloads in its own goroutine — bounded
// within itself by parallel_files — so one huge torrent no longer holds
// already-cached content hostage behind it. The pass only spawns and returns;
// completion is observed via the store.
func (e *Engine) downloadPass(ctx context.Context) error {
	resuming, err := e.store.ListByState(ctx, store.StateLocalDload)
	if err != nil {
		return err
	}
	queued, err := e.store.ListByState(ctx, store.StateLocalQueued)
	if err != nil {
		return err
	}
	for _, t := range append(resuming, queued...) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.maybeStartDownload(ctx, t)
	}
	return nil
}

// maybeStartDownload spawns downloadOne for t unless it is already being
// downloaded or all parallelTorrents slots are busy. Registration in
// activeDownloads both bounds concurrency and lets DeleteTorrent cancel the
// right download; a torrent stays registered until its goroutine exits, so the
// short pass cadence can never double-start one.
func (e *Engine) maybeStartDownload(ctx context.Context, t store.Torrent) {
	e.mu.Lock()
	if _, running := e.activeDownloads[t.Infohash]; running || len(e.activeDownloads) >= e.parallelTorrents {
		e.mu.Unlock()
		return
	}
	if r, ok := e.localRetries[t.Infohash]; ok && time.Now().Before(r.next) {
		e.mu.Unlock()
		return // backing off after a failed attempt (localFailed)
	}
	// Cancellable sub-context so a concurrent delete can abort just this
	// download without tearing down the rest of the engine.
	dlCtx, cancel := context.WithCancel(ctx)
	e.activeDownloads[t.Infohash] = cancel
	e.mu.Unlock()

	e.dlWG.Add(1)
	go func() {
		defer e.dlWG.Done()
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.activeDownloads, t.Infohash)
			e.mu.Unlock()
		}()
		e.downloadOne(ctx, dlCtx, t)
	}()
}

// downloadOne fetches all of a torrent's files into a staging tree, atomically
// moves the content into place, and marks the torrent COMPLETE. Any hard failure
// moves it to ERROR; a shutdown (ctx cancelled) or per-torrent cancellation
// (dlCtx cancelled via Engine.DeleteTorrent) leaves it for a later resume.
func (e *Engine) downloadOne(ctx, dlCtx context.Context, t store.Torrent) {
	if !t.TorBoxID.Valid {
		e.fail(ctx, t, "no TorBox id for download")
		return
	}
	files, err := e.store.ListFiles(ctx, t.ID)
	if err != nil {
		e.log.Error("list files", "infohash", t.Infohash, "err", err)
		return
	}
	if len(files) == 0 {
		e.fail(ctx, t, "no files to download")
		return
	}
	if t.State == store.StateLocalQueued {
		if err := e.store.MarkLocalDownloading(ctx, t.ID); err != nil {
			e.log.Error("mark local downloading", "infohash", t.Infohash, "err", err)
			return
		}
	}

	rels := make([]string, 0, len(files))
	for _, f := range files {
		rels = append(rels, f.RelPath)
	}
	// toLocalQueued already rejects these; rows recorded by an older build
	// weren't checked, so re-check before joining onto the staging path.
	if bad, ok := unsafeRelPath(rels); ok {
		e.fail(ctx, t, fmt.Sprintf("unsafe file path in torrent: %q", bad))
		return
	}

	staging := filepath.Join(t.SavePath, e.incomplete, t.Infohash)
	var totalSize int64
	jobs := make([]downloader.File, 0, len(files))
	for _, f := range files {
		totalSize += f.Size
		jobs = append(jobs, downloader.File{
			RowID:    f.ID,
			TorBoxID: int(f.TorBoxFileID.Int64),
			Dest:     filepath.Join(staging, f.RelPath),
			Size:     f.Size,
		})
	}
	if totalSize == 0 {
		totalSize = t.Size
	}

	torboxID := int(t.TorBoxID.Int64)
	link := func(ctx context.Context, fileID int) (string, error) {
		fid := fileID
		return e.torbox.RequestDL(ctx, torbox.RequestDLParams{TorrentID: torboxID, FileID: &fid})
	}

	prevBytes := int64(0)
	prevTime := time.Now()
	// The first progress sample carries the bytes already on disk (seeded by the
	// downloader on resume). Measuring speed against a zero baseline would report
	// a huge one-tick spike, so the first sample only sets the baseline.
	firstSample := true
	progress := func(downloaded int64) {
		now := time.Now()
		var speed int64
		if !firstSample {
			if dt := now.Sub(prevTime).Seconds(); dt > 0 {
				if s := int64(float64(downloaded-prevBytes) / dt); s > 0 {
					speed = s
				}
			}
		}
		firstSample = false
		prevBytes, prevTime = downloaded, now
		var p float64
		if totalSize > 0 {
			p = float64(downloaded) / float64(totalSize)
			if p > 1 {
				p = 1
			}
		}
		if err := e.store.UpdateLocalProgress(ctx, t.ID, p, speed); err != nil && ctx.Err() == nil {
			e.log.Warn("update local progress", "infohash", t.Infohash, "err", err)
		}
	}
	fileDone := func(rowID, size int64) {
		if err := e.store.MarkFileDone(ctx, rowID, size); err != nil && ctx.Err() == nil {
			e.log.Warn("mark file done", "infohash", t.Infohash, "err", err)
		}
	}

	e.log.Info("downloading", "infohash", t.Infohash, "name", t.Name, "files", len(files), "size", totalSize)
	err = downloader.Download(dlCtx, jobs, link, downloader.Options{
		Parallel:    e.parallel,
		HTTPClient:  e.httpClient,
		Progress:    progress,
		FileDone:    fileDone,
		IdleTimeout: e.idleTimeout,
	})
	if err != nil {
		if dlCtx.Err() != nil && ctx.Err() == nil {
			// Per-torrent cancellation (e.g. delete while downloading): keep
			// state for potential resume — the deleter will drop the row.
			return
		}
		if dlCtx.Err() != nil {
			return // engine shutting down
		}
		e.localFailed(ctx, t, "download: "+err.Error(), err)
		return
	}

	contentPath, err := downloader.Finalize(staging, t.SavePath)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.localFailed(ctx, t, "finalize: "+err.Error(), err)
		return
	}
	e.mu.Lock()
	delete(e.localRetries, t.Infohash)
	e.mu.Unlock()
	if err := e.store.MarkComplete(ctx, t.ID, contentPath, totalSize); err != nil {
		e.log.Error("mark complete", "infohash", t.Infohash, "err", err)
		return
	}
	e.log.Info("download complete", "infohash", t.Infohash, "content_path", contentPath)

	// Delete from TorBox now that the content is safely on local disk: we no longer
	// need TorBox's copy (Sonarr imports from /downloads, not TorBox), so dropping it
	// keeps the account's list tidy. This isn't about freeing a slot — with seeding
	// disabled the torrent went inactive the moment it finished caching and already
	// holds none. We keep the local files and the DB row (reported pausedUP) so Sonarr
	// can still import; the later Sonarr-driven delete then just clears local state,
	// with this TorBox delete already a no-op. Best-effort and detached so it
	// completes regardless of the pass context.
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	e.removeFromTorBox(delCtx, t)
	cancel()
}

// Local retry schedule: the delay before attempt n+1 after n failures doubles
// from localRetryBase up to localRetryMax, and the torrent is failed once
// maxLocalAttempts attempts have failed (about 1.5h with the defaults). Vars so
// tests can shrink them.
var (
	localRetryBase   = time.Minute
	localRetryMax    = 30 * time.Minute
	maxLocalAttempts = 8
)

// localFailed handles a failed local download (or finalize). By this point
// TorBox has the content cached, so the failure is almost never the release's
// fault — a CDN outage, requestdl rate limiting, a full disk — and failing the
// torrent would make the *arr push-back blocklist a good release. So the torrent
// keeps its LOCAL_* state and is retried with exponential backoff, resuming from
// the bytes already on disk; only after maxLocalAttempts failed attempts is it
// failed for real. A full or read-only disk is never the release's fault at all,
// so those failures retry without consuming the budget. The reason is noted on
// the row so the dashboard shows why a download is waiting.
func (e *Engine) localFailed(ctx context.Context, t store.Torrent, reason string, cause error) {
	diskErr := errors.Is(cause, syscall.ENOSPC) || errors.Is(cause, syscall.EROFS)

	e.mu.Lock()
	r := e.localRetries[t.Infohash]
	if !diskErr {
		r.attempts++
	}
	giveUp := r.attempts >= maxLocalAttempts
	var wait time.Duration
	if giveUp {
		delete(e.localRetries, t.Infohash)
	} else {
		wait = localBackoff(r.attempts)
		r.next = time.Now().Add(wait)
		e.localRetries[t.Infohash] = r
	}
	e.mu.Unlock()

	if giveUp {
		e.fail(ctx, t, fmt.Sprintf("%s (after %d attempts)", reason, r.attempts))
		return
	}
	e.log.Warn("local download failed; will retry",
		"infohash", t.Infohash, "attempt", r.attempts, "max_attempts", maxLocalAttempts,
		"retry_in", wait, "disk_error", diskErr, "err", reason)
	note := fmt.Sprintf("retrying in %s: %s", wait.Round(time.Second), reason)
	if err := e.store.NoteLocalRetry(ctx, t.ID, note); err != nil && ctx.Err() == nil {
		e.log.Warn("note local retry", "infohash", t.Infohash, "err", err)
	}
}

// localBackoff returns the wait after the given number of failed attempts.
func localBackoff(attempts int) time.Duration {
	d := localRetryBase
	for i := 1; i < attempts && d < localRetryMax; i++ {
		d *= 2
	}
	if d > localRetryMax {
		d = localRetryMax
	}
	return d
}
