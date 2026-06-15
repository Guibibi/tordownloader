package engine

import (
	"context"
	"path/filepath"
	"time"

	"github.com/guibibi/tordownloader/internal/downloader"
	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// downloadPass pulls every torrent whose content is present on TorBox to local
// disk. It resumes in-flight downloads (LOCAL_DOWNLOAD) before starting newly
// ready ones (LOCAL_QUEUED), and processes them one at a time so total
// connection use stays bounded by parallel-files within a single torrent.
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
		e.downloadOne(ctx, t)
	}
	return nil
}

// downloadOne fetches all of a torrent's files into a staging tree, atomically
// moves the content into place, and marks the torrent COMPLETE. Any hard failure
// moves it to ERROR; a shutdown (ctx cancelled) or per-torrent cancellation
// (via Engine.DeleteTorrent) leaves it for a later resume.
func (e *Engine) downloadOne(ctx context.Context, t store.Torrent) {
	// Create a cancellable sub-context so a concurrent delete can cancel just
	// this download without tearing down the rest of the engine.
	dlCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.activeDownload = &activeDownload{cancel: cancel, infohash: t.Infohash, torrentID: t.ID}
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		if e.activeDownload != nil && e.activeDownload.infohash == t.Infohash {
			e.activeDownload = nil
		}
		e.mu.Unlock()
	}()
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
		Parallel:   e.parallel,
		HTTPClient: e.httpClient,
		Progress:   progress,
		FileDone:   fileDone,
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
		e.fail(ctx, t, "download: "+err.Error())
		return
	}

	contentPath, err := downloader.Finalize(staging, t.SavePath)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.fail(ctx, t, "finalize: "+err.Error())
		return
	}
	if err := e.store.MarkComplete(ctx, t.ID, contentPath, totalSize); err != nil {
		e.log.Error("mark complete", "infohash", t.Infohash, "err", err)
		return
	}
	e.log.Info("download complete", "infohash", t.Infohash, "content_path", contentPath)

	// Free the TorBox slot now that the content is safely on local disk. TorBox no
	// longer needs to seed it — Sonarr imports from /downloads, not TorBox — and
	// waiting for Sonarr's delete (which may be disabled, or deferred while the
	// torrent "seeds") would pin one of the account's scarce slots, starving the
	// queued torrents. We keep the local files and the DB row (reported pausedUP)
	// so Sonarr can still import; the later Sonarr-driven delete then just clears
	// local state, with this TorBox delete already a no-op. Best-effort and
	// detached so it completes regardless of the pass context.
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	e.removeFromTorBox(delCtx, t)
	cancel()
}
