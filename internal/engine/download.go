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
// moves it to ERROR; a shutdown (ctx cancelled) leaves it for a later resume.
func (e *Engine) downloadOne(ctx context.Context, t store.Torrent) {
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
	progress := func(downloaded int64) {
		now := time.Now()
		var speed int64
		if dt := now.Sub(prevTime).Seconds(); dt > 0 {
			if s := int64(float64(downloaded-prevBytes) / dt); s > 0 {
				speed = s
			}
		}
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
	err = downloader.Download(ctx, jobs, link, downloader.Options{
		Parallel:   e.parallel,
		HTTPClient: e.httpClient,
		Progress:   progress,
		FileDone:   fileDone,
	})
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down: keep state for resume
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
}
