package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// seedLocalQueued adds a torrent, drives it to LOCAL_QUEUED, and records its
// files — the state the downloader picks up.
func seedLocalQueued(t *testing.T, st *store.Store, hash, savePath string, files []store.FileInput) store.Torrent {
	t.Helper()
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
	cp := filepath.Join(savePath, "show")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, files); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	got, _, err := st.GetTorrent(ctx, hash)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

func TestDownloadPassCompletes(t *testing.T) {
	e1 := bytes.Repeat([]byte("1"), 1200)
	e2 := bytes.Repeat([]byte("2"), 800)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/0":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(e1))
		case "/1":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(e2))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: int64(len(e1))},
		{TorBoxFileID: 1, RelPath: "show/e02.mkv", ShortName: "e02.mkv", Size: int64(len(e2))},
	})

	tb := &fakeTB{dl: func(p torbox.RequestDLParams) (string, error) {
		if p.TorrentID != 4242 || p.FileID == nil {
			t.Errorf("unexpected requestdl params: %+v", p)
		}
		return srv.URL + "/" + strconv.Itoa(*p.FileID), nil
	}}
	e := New(st, tb, Config{ParallelFiles: 2, IncompleteSubdir: ".incomplete"}, nil)

	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait() // downloads run in goroutines; wait before asserting

	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE", tr.State)
	}
	if tr.LocalProgress != 1 {
		t.Errorf("local_progress = %v, want 1", tr.LocalProgress)
	}
	if tr.CompletedOn == 0 {
		t.Error("completed_on not set")
	}
	wantCP := filepath.Join(save, "show")
	if tr.ContentPath != wantCP {
		t.Errorf("content_path = %q, want %q", tr.ContentPath, wantCP)
	}
	assertBytes(t, filepath.Join(save, "show", "e01.mkv"), e1)
	assertBytes(t, filepath.Join(save, "show", "e02.mkv"), e2)

	// Files marked done.
	rows, _ := st.ListFiles(context.Background(), tr.ID)
	for _, f := range rows {
		if !f.Done {
			t.Errorf("file %s not marked done", f.RelPath)
		}
	}
}

// TestDownloadPassDeletesFromTorBoxOnComplete asserts that once the content is on
// local disk, the engine deletes the torrent from TorBox to free the account's
// scarce slot — without waiting for Sonarr's delete (which may be disabled).
func TestDownloadPassDeletesFromTorBoxOnComplete(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "cccccccccccccccccccccccccccccccccccccccc"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: int64(len(body))},
	})

	var ctlID int
	var ctlOp torbox.Operation
	var ctlCalled int
	tb := &fakeTB{
		dl:  func(torbox.RequestDLParams) (string, error) { return srv.URL, nil },
		ctl: func(id int, op torbox.Operation) error { ctlCalled++; ctlID = id; ctlOp = op; return nil },
	}
	e := New(st, tb, Config{ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait() // downloads run in goroutines; wait before asserting

	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE", tr.State)
	}
	// Slot freed on TorBox, but local content stays so Sonarr can still import.
	if ctlCalled != 1 || ctlOp != torbox.OpDelete || ctlID != 4242 {
		t.Errorf("ControlTorrent calls=%d id=%d op=%q, want 1 delete on id 4242", ctlCalled, ctlID, ctlOp)
	}
	assertBytes(t, filepath.Join(save, "show", "e01.mkv"), body)
}

// shrinkLocalRetries swaps the local retry schedule for the test's duration.
func shrinkLocalRetries(t *testing.T, base time.Duration, attempts int) {
	t.Helper()
	oldBase, oldMax, oldAttempts := localRetryBase, localRetryMax, maxLocalAttempts
	localRetryBase, localRetryMax, maxLocalAttempts = base, base, attempts
	t.Cleanup(func() { localRetryBase, localRetryMax, maxLocalAttempts = oldBase, oldMax, oldAttempts })
}

// TestDownloadPassRetriesLocalFailureBeforeFailing asserts that a failed local
// download is not failed outright — TorBox already has the content, so a CDN or
// disk problem must not make the *arr push-back blocklist a good release. It
// stays LOCAL_DOWNLOAD with a retry note, is not restarted before its backoff
// elapses, and is failed only once the attempt budget is spent.
func TestDownloadPassRetriesLocalFailureBeforeFailing(t *testing.T) {
	shrinkLocalRetries(t, time.Hour, 2)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1) // a non-retryable status fails the attempt immediately
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", Size: 100},
	})
	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) { return srv.URL, nil }}
	e := New(st, tb, Config{ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	pass := func() {
		t.Helper()
		if err := e.downloadPass(context.Background()); err != nil {
			t.Fatalf("downloadPass: %v", err)
		}
		e.dlWG.Wait()
	}

	pass()
	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateLocalDload {
		t.Fatalf("state after first failure = %q, want LOCAL_DOWNLOAD (retry, not ERROR)", tr.State)
	}
	if !strings.Contains(tr.Error, "retrying in") {
		t.Errorf("error note = %q, want a retry note for the dashboard", tr.Error)
	}

	pass() // still inside the 1h backoff: must not restart
	if hits.Load() != 1 {
		t.Fatalf("CDN hits = %d, want 1 (no attempt before the backoff elapses)", hits.Load())
	}

	e.mu.Lock()
	r := e.localRetries[hash]
	r.next = time.Now().Add(-time.Second) // backoff elapsed
	e.localRetries[hash] = r
	e.mu.Unlock()

	pass() // second failed attempt exhausts the budget of 2
	tr, _, _ = st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateError {
		t.Fatalf("state after budget spent = %q, want ERROR", tr.State)
	}
	if !strings.Contains(tr.Error, "after 2 attempts") {
		t.Errorf("error = %q, want it to say how many attempts failed", tr.Error)
	}
}

// TestDownloadPassRetrySucceeds asserts that a download which fails once and
// then works completes normally, with the retry note cleared.
func TestDownloadPassRetrySucceeds(t *testing.T) {
	shrinkLocalRetries(t, time.Nanosecond, 3)
	body := bytes.Repeat([]byte("r"), 300)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "dddddddddddddddddddddddddddddddddddddddd"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", Size: int64(len(body))},
	})
	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) { return srv.URL, nil }}
	e := New(st, tb, Config{ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	for i := 0; i < 2; i++ {
		if err := e.downloadPass(context.Background()); err != nil {
			t.Fatalf("downloadPass: %v", err)
		}
		e.dlWG.Wait()
	}
	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE after a successful retry", tr.State)
	}
	if tr.Error != "" {
		t.Errorf("error = %q, want the retry note cleared on completion", tr.Error)
	}
	e.mu.Lock()
	_, left := e.localRetries[hash]
	e.mu.Unlock()
	if left {
		t.Error("retry bookkeeping not cleared after success")
	}
}

// TestLocalFailedDiskErrorDoesNotSpendBudget asserts that a full disk — never
// the release's fault — is retried without counting toward the attempt budget,
// so it can never end in a blocklist.
func TestLocalFailedDiskErrorDoesNotSpendBudget(t *testing.T) {
	shrinkLocalRetries(t, time.Hour, 1)
	st := newStore(t)
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	tr := seedLocalQueued(t, st, hash, filepath.Join(t.TempDir(), "tv"), []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", Size: 100},
	})
	e := New(st, &fakeTB{}, Config{}, nil)

	cause := fmt.Errorf("e01.mkv: %w", &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC})
	for i := 0; i < 3; i++ {
		e.localFailed(context.Background(), tr, "download: disk full", cause)
	}
	got, _, _ := st.GetTorrent(context.Background(), hash)
	if got.State == store.StateError {
		t.Fatalf("state = ERROR; a full disk must never exhaust the retry budget")
	}
	e.mu.Lock()
	r := e.localRetries[hash]
	e.mu.Unlock()
	if r.attempts != 0 || r.next.IsZero() {
		t.Errorf("retry state = %+v, want 0 counted attempts and a scheduled retry", r)
	}
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %d bytes, want %d", path, len(got), len(want))
	}
}

// TestDownloadPassParallelTorrents asserts that up to parallel_torrents
// downloads run concurrently, no torrent is double-started by repeated passes,
// and torrents beyond the limit wait for a free slot.
func TestDownloadPassParallelTorrents(t *testing.T) {
	release := make(chan struct{})
	body := bytes.Repeat([]byte("p"), 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold every download open until the test releases them
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hashes := []string{
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333",
		"4444444444444444444444444444444444444444",
	}
	for _, h := range hashes {
		seedLocalQueued(t, st, h, save, []store.FileInput{
			{TorBoxFileID: 0, RelPath: "show-" + h[:4] + "/e01.mkv", ShortName: "e01.mkv", Size: int64(len(body))},
		})
	}

	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) { return srv.URL, nil }}
	e := New(st, tb, Config{ParallelTorrents: 3, ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	countActive := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.activeDownloads)
	}

	// Two passes back to back: the limit must hold and nothing double-starts.
	for i := 0; i < 2; i++ {
		if err := e.downloadPass(context.Background()); err != nil {
			t.Fatalf("downloadPass: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for countActive() != 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countActive(); got != 3 {
		t.Fatalf("active downloads = %d, want 3 (the limit)", got)
	}

	close(release)
	e.dlWG.Wait()

	// Three done, the fourth still waiting its turn; another pass finishes it.
	if n := countState(t, st, store.StateComplete); n != 3 {
		t.Fatalf("complete after first wave = %d, want 3", n)
	}
	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait()
	if n := countState(t, st, store.StateComplete); n != 4 {
		t.Fatalf("complete after second wave = %d, want 4", n)
	}
}

// TestDownloadRejectsUnsafePathFromOldRow asserts the downloader re-checks file
// paths before joining them onto the staging dir: rows recorded by an older
// build were never validated, and "../" would write outside it.
func TestDownloadRejectsUnsafePathFromOldRow(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	st := newStore(t)
	root := t.TempDir()
	save := filepath.Join(root, "tv")
	hash := "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "../../../escaped.mkv", Size: 1},
	})
	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) { return srv.URL, nil }}
	e := New(st, tb, Config{ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)
	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait()

	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateError || !strings.Contains(tr.Error, "unsafe file path") {
		t.Fatalf("state = %q error = %q, want ERROR for an unsafe path", tr.State, tr.Error)
	}
	if hits.Load() != 0 {
		t.Errorf("CDN hits = %d, want 0 (nothing fetched)", hits.Load())
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.mkv")); !os.IsNotExist(err) {
		t.Errorf("file written outside the download dir (stat err = %v)", err)
	}
}

// TestDiskSpaceGateHoldsDownloadUntilSpaceFrees asserts a torrent that would
// not fit (remaining bytes + reserve > free) is not started and not failed: it
// stays LOCAL_QUEUED with a note explaining the wait, and starts once enough
// space is reported, clearing the note.
func TestDiskSpaceGateHoldsDownloadUntilSpaceFrees(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "dddddddddddddddddddddddddddddddddddddddd"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: int64(len(body))},
	})
	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) { return srv.URL + "/f", nil }}
	e := New(st, tb, Config{MinFreeSpace: 500}, nil)

	var free atomic.Uint64
	free.Store(1200) // 1000 to write + 500 reserve doesn't fit
	var measured atomic.Value
	e.diskFree = func(p string) (uint64, bool) { measured.Store(p); return free.Load(), true }

	ctx := context.Background()
	if err := e.downloadPass(ctx); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait()
	tr, _, _ := st.GetTorrent(ctx, hash)
	if tr.State != store.StateLocalQueued {
		t.Fatalf("state = %q, want LOCAL_QUEUED while waiting for space", tr.State)
	}
	if !strings.Contains(tr.Error, "waiting for disk space") {
		t.Errorf("error note = %q, want a disk-space wait note", tr.Error)
	}
	if got, _ := measured.Load().(string); got != save {
		t.Errorf("measured free space at %q, want the save path %q", got, save)
	}

	free.Store(1500) // now exactly fits
	if err := e.downloadPass(ctx); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait()
	tr, _, _ = st.GetTorrent(ctx, hash)
	if tr.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE once space frees", tr.State)
	}
	if tr.Error != "" {
		t.Errorf("error note = %q, want it cleared", tr.Error)
	}
}

// TestDiskSpaceGateCountsInFlightDownloads asserts the gate reserves room for
// what running downloads still have to write, so two downloads that each fit
// alone aren't both started onto a disk that can only hold one.
func TestDiskSpaceGateCountsInFlightDownloads(t *testing.T) {
	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	tr := seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: 1000},
	})
	e := New(st, &fakeTB{}, Config{}, nil)
	e.diskFree = func(string) (uint64, bool) { return 1500, true }

	ctx := context.Background()
	if _, ok := e.diskSpaceFor(ctx, tr); !ok {
		t.Fatal("download should fit with nothing else running")
	}
	e.outstanding["ffffffffffffffffffffffffffffffffffffffff"] = 600 // another download still has 600 to write
	if _, ok := e.diskSpaceFor(ctx, tr); ok {
		t.Fatal("download should wait: 1000 + 600 in flight > 1500 free")
	}

	// A partial already on disk counts toward what's written.
	partial := filepath.Join(save, ".incomplete", hash, "show", "e01.mkv")
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, make([]byte, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	if remaining, ok := e.diskSpaceFor(ctx, tr); !ok || remaining != 800 {
		t.Fatalf("diskSpaceFor = (%d, %v), want (800, true): 800 left + 600 in flight fits 1500", remaining, ok)
	}

	// A negative reserve disables the gate entirely.
	e.minFreeSpace = -1
	e.diskFree = func(string) (uint64, bool) { return 0, true }
	if _, ok := e.diskSpaceFor(ctx, tr); !ok {
		t.Fatal("a disabled gate must never hold a download")
	}
}
