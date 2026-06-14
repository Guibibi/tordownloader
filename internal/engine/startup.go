package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/guibibi/tordownloader/internal/store"
)

// startupReconcile runs once at engine start to recover state from a previous
// run that may have crashed mid-operation. It:
//
//  1. Re-syncs every TORBOX_ACTIVE torrent against TorBox (mylist). While we
//     were down, content may have become available, or a torrent may have
//     vanished / exceeded the fail-fast window.
//
//  2. Recovers any LOCAL_QUEUED or LOCAL_DOWNLOAD torrents whose content is
//     already at the final location — Finalize ran before a crash, but
//     MarkComplete was never called. Without this check the downloader would
//     re-fetch everything (or fail on an empty staging dir), and the torrent
//     might sit in the wrong state indefinitely.
//
// After startup reconciliation completes, the periodic loops take over and
// resume normal operation.
func (e *Engine) startupReconcile(ctx context.Context) error {
	e.log.Info("startup reconciliation started")

	// 1. Re-sync TORBOX_ACTIVE torrents against the live TorBox account.
	//    The reconciler handles vanish, fail-fast, and download_present →
	//    LOCAL_QUEUED transitions.
	if err := e.reconcilePass(ctx); err != nil {
		// mylist may fail temporarily; the periodic reconciler will retry.
		e.log.Warn("startup reconcile pass had errors, periodic reconciler will retry", "err", err)
	}

	// 2. Recover torn-local-completes from LOCAL_QUEUED / LOCAL_DOWNLOAD.
	for _, state := range []string{store.StateLocalDload, store.StateLocalQueued} {
		torrents, err := e.store.ListByState(ctx, state)
		if err != nil {
			return fmt.Errorf("startup: list %s: %w", state, err)
		}
		for _, t := range torrents {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.recoverIfComplete(ctx, t)
		}
	}

	e.log.Info("startup reconciliation complete")
	return nil
}

// recoverIfComplete checks whether all of a torrent's files are already at
// their final location (<save_path>/<rel_path>). When Finalize succeeded but
// MarkComplete didn't (crash), the files are on disk at the final location but
// the DB still says LOCAL_DOWNLOAD. This method detects that and marks the
// torrent COMPLETE so Sonarr can import without any re-download.
func (e *Engine) recoverIfComplete(ctx context.Context, t store.Torrent) {
	files, err := e.store.ListFiles(ctx, t.ID)
	if err != nil {
		e.log.Error("recovery: list files", "infohash", t.Infohash, "err", err)
		return
	}
	if len(files) == 0 {
		return
	}

	// Verify every file is present at its final location with the expected size.
	for _, f := range files {
		finalPath := filepath.Join(t.SavePath, f.RelPath)
		fi, err := os.Stat(finalPath)
		if err != nil {
			return // file missing — content is not fully in place
		}
		if f.Size > 0 && fi.Size() != f.Size {
			e.log.Warn("recovery: size mismatch, content will be re-downloaded",
				"infohash", t.Infohash, "file", f.RelPath,
				"on_disk", fi.Size(), "expected", f.Size)
			return
		}
	}

	// All files are present and correct. Mark COMPLETE so Sonarr can import.
	cp := contentPathFromFiles(t.SavePath, t.Name, files)
	var totalSize int64
	for _, f := range files {
		totalSize += f.Size
	}

	if err := e.store.MarkComplete(ctx, t.ID, cp, totalSize); err != nil {
		e.log.Error("recovery: mark complete", "infohash", t.Infohash, "err", err)
		return
	}
	e.log.Info("recovered: content already on disk from prior run, marked complete",
		"infohash", t.Infohash, "content_path", cp)
}

// contentPathFromFiles computes the qBittorrent content_path from the stored
// files list (the same logic as reconcile.go:contentPath but operating on
// store.File instead of store.FileInput).
func contentPathFromFiles(savePath, name string, files []store.File) string {
	if len(files) == 1 {
		return filepath.Join(savePath, files[0].RelPath)
	}
	if name != "" {
		return filepath.Join(savePath, name)
	}
	return savePath
}
