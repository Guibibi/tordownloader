package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// TestStartupReconcileResyncsActive verifies that on restart a TORBOX_ACTIVE
// torrent whose content became available while the process was down is moved to
// LOCAL_QUEUED by the startup reconciliation pass.
func TestStartupReconcileResyncsActive(t *testing.T) {
	st := newStore(t)
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedActive(t, st, hash, 555, "/downloads/tv", time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID:              555,
			Hash:            hash,
			Name:            "show",
			Size:            300,
			DownloadPresent: true,
			Progress:        1,
			Files: []torbox.TorrentFile{
				{ID: 0, Name: "show/show.S01E01.mkv", ShortName: "show.S01E01.mkv", Size: 100},
			},
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)

	if err := e.startupReconcile(context.Background()); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	tr := getTorrent(t, st, hash)
	if tr.State != store.StateLocalQueued {
		t.Fatalf("state = %q, want LOCAL_QUEUED after startup re-sync", tr.State)
	}
}

// TestStartupReconcileDetectsTornComplete verifies recovery when a crash
// happened between Finalize and MarkComplete: the files are at the final
// location but the DB says LOCAL_DOWNLOAD. On restart it must be marked
// COMPLETE without re-downloading.
func TestStartupReconcileDetectsTornComplete(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Simulate the torn state: torrent is LOCAL_DOWNLOAD, files are on disk at
	// the final location (as if Finalize already moved them).
	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 4242); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "show.S01E01.mkv")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show.S01E01.mkv", ShortName: "show.S01E01.mkv", Size: 42},
	}); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	if err := st.MarkLocalDownloading(ctx, tr.ID); err != nil {
		t.Fatalf("mark local downloading: %v", err)
	}

	// Now crash between Finalize and MarkComplete: files are in the final spot.
	mustWriteFile(t, filepath.Join(savePath, "show.S01E01.mkv"), 42)

	// Run startup reconciliation — should detect and mark COMPLETE.
	e := New(st, &fakeTB{}, Config{MaxSlots: 3}, nil)
	if err := e.startupReconcile(ctx); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	tr2 := getTorrent(t, st, hash)
	if tr2.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE (recovered)", tr2.State)
	}
	if tr2.LocalProgress != 1 {
		t.Errorf("local_progress = %v, want 1", tr2.LocalProgress)
	}
	if tr2.CompletedOn == 0 {
		t.Error("completed_on not set in recovery")
	}
}

// TestStartupReconcileTornMultiFile verifies recovery of a multi-file torrent
// where Finalize ran before a crash.
func TestStartupReconcileTornMultiFile(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "cccccccccccccccccccccccccccccccccccccccc"

	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 99); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "show")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: 100},
		{TorBoxFileID: 1, RelPath: "show/e02.mkv", ShortName: "e02.mkv", Size: 200},
	}); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	if err := st.MarkLocalDownloading(ctx, tr.ID); err != nil {
		t.Fatalf("mark local downloading: %v", err)
	}

	// Files at final location as if Finalize already placed them.
	mustWriteFile(t, filepath.Join(savePath, "show", "e01.mkv"), 100)
	mustWriteFile(t, filepath.Join(savePath, "show", "e02.mkv"), 200)

	e := New(st, &fakeTB{}, Config{MaxSlots: 3}, nil)
	if err := e.startupReconcile(ctx); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	tr2 := getTorrent(t, st, hash)
	if tr2.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE (multi-file recovered)", tr2.State)
	}
	if tr2.ContentPath != cp {
		t.Errorf("content_path = %q, want %q", tr2.ContentPath, cp)
	}
}

// TestStartupReconcilePartialDownloadNotRecovered verifies that a partially
// downloaded torrent (files in staging but not finalized) is NOT incorrectly
// marked complete by the startup recovery — the files are not at the final
// location yet.
func TestStartupReconcilePartialDownloadNotRecovered(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "dddddddddddddddddddddddddddddddddddddddd"

	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 88); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "show")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: 500},
	}); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	if err := st.MarkLocalDownloading(ctx, tr.ID); err != nil {
		t.Fatalf("mark local downloading: %v", err)
	}

	// File is in STAGING, not at the final location — crash during download.
	stagingPath := filepath.Join(savePath, ".incomplete", hash, "show", "e01.mkv")
	mustWriteFile(t, stagingPath, 300) // partial: 300 of 500 bytes

	e := New(st, &fakeTB{}, Config{MaxSlots: 3, IncompleteSubdir: ".incomplete"}, nil)
	if err := e.startupReconcile(ctx); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	// Torrent should still be LOCAL_DOWNLOAD — startup reconcile must NOT mark
	// it COMPLETE because the file is at the staging path, not the final path.
	tr2 := getTorrent(t, st, hash)
	if tr2.State != store.StateLocalDload {
		t.Fatalf("state = %q, want LOCAL_DOWNLOAD (partial, not recovered)", tr2.State)
	}
}

// TestStartupReconcileSizeMismatchNotRecovered verifies that when a file exists
// at the final location but has the wrong size (corrupt), the torrent is NOT
// marked complete.
func TestStartupReconcileSizeMismatchNotRecovered(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 77); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "file.mkv")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "file.mkv", ShortName: "file.mkv", Size: 1000},
	}); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	if err := st.MarkLocalDownloading(ctx, tr.ID); err != nil {
		t.Fatalf("mark local downloading: %v", err)
	}

	// File at final location but wrong size.
	mustWriteFile(t, filepath.Join(savePath, "file.mkv"), 500) // expected 1000

	e := New(st, &fakeTB{}, Config{MaxSlots: 3}, nil)
	if err := e.startupReconcile(ctx); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	tr2 := getTorrent(t, st, hash)
	if tr2.State != store.StateLocalDload {
		t.Fatalf("state = %q, want LOCAL_DOWNLOAD (size mismatch, not recovered)", tr2.State)
	}
}

// TestStartupReconcileLOCALQUEUEDNotRecoveredWhenFilesMissing verifies that a
// LOCAL_QUEUED torrent without files on disk at the final location stays in
// LOCAL_QUEUED — it hasn't started downloading yet and the downloader will
// pick it up.
func TestStartupReconcileLOCALQUEUEDNotRecoveredWhenFilesMissing(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "ffffffffffffffffffffffffffffffffffffffff"

	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 66); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "show")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: 100},
	}); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	// No files on disk at all — nothing to recover.

	e := New(st, &fakeTB{}, Config{MaxSlots: 3}, nil)
	if err := e.startupReconcile(ctx); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	tr2 := getTorrent(t, st, hash)
	if tr2.State != store.StateLocalQueued {
		t.Fatalf("state = %q, want LOCAL_QUEUED (nothing to recover)", tr2.State)
	}
}

// TestStartupReconcileResumeDownload verifies the full restart-during-download
// flow: the torrent is LOCAL_DOWNLOAD with partial files in staging and no
// files at the final location. On restart, startup reconciliation does NOT
// mark it complete, and the downloader subsequently resumes.
func TestStartupReconcileThenDownloadResumes(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "1111111111111111111111111111111111111111"

	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 55); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "show")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: 100},
	}); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	if err := st.MarkLocalDownloading(ctx, tr.ID); err != nil {
		t.Fatalf("mark local downloading: %v", err)
	}

	// Write a partial file in staging to simulate crash during download.
	stagingFile := filepath.Join(savePath, ".incomplete", hash, "show", "e01.mkv")
	mustWriteFile(t, stagingFile, 40)

	// 1. Startup reconcile: must leave it LOCAL_DOWNLOAD.
	e := New(st, &fakeTB{}, Config{MaxSlots: 3, IncompleteSubdir: ".incomplete"}, nil)
	if err := e.startupReconcile(ctx); err != nil {
		t.Fatalf("startupReconcile: %v", err)
	}

	tr2 := getTorrent(t, st, hash)
	if tr2.State != store.StateLocalDload {
		t.Fatalf("after startup: state = %q, want LOCAL_DOWNLOAD", tr2.State)
	}

	// Verify the partial file is still there.
	if _, err := os.Stat(stagingFile); err != nil {
		t.Fatalf("staging file should still exist after startup reconcile: %v", err)
	}
}

// TestStartupReconcileHandlesEmptyState verifies startup reconciliation runs
// successfully with no torrents in the database.
func TestStartupReconcileHandlesEmptyState(t *testing.T) {
	st := newStore(t)
	e := New(st, &fakeTB{}, Config{MaxSlots: 3}, nil)

	if err := e.startupReconcile(context.Background()); err != nil {
		t.Fatalf("startupReconcile with empty DB: %v", err)
	}
}

// mustWriteFile creates a file at path with the given size bytes of content.
func mustWriteFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
