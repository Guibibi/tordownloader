package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// fakeTB is a scriptable TorBoxAPI.
type fakeTB struct {
	mu    sync.Mutex
	calls int
	fn    func(req torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error)
	list  func() ([]torbox.Torrent, error)
	dl    func(p torbox.RequestDLParams) (string, error)
	ctl   func(torrentID int, op torbox.Operation) error
}

func (f *fakeTB) CreateTorrent(_ context.Context, r torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.fn(r)
}

func (f *fakeTB) MyList(_ context.Context, _ bool) ([]torbox.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.list == nil {
		return nil, nil
	}
	return f.list()
}

func (f *fakeTB) RequestDL(_ context.Context, p torbox.RequestDLParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dl == nil {
		return "", nil
	}
	return f.dl(p)
}

func (f *fakeTB) ControlTorrent(_ context.Context, torrentID int, op torbox.Operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctl == nil {
		return nil
	}
	return f.ctl(torrentID, op)
}

func okResult(id int) (*torbox.CreateTorrentResult, error) {
	return &torbox.CreateTorrentResult{TorrentID: &id}, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedQueued adds n QUEUED torrents, each with a dummy magnet source.
func seedQueued(t *testing.T, st *store.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("%040x", i+1)
		_, _, err := st.AddTorrent(context.Background(), store.AddTorrentParams{
			Infohash: hash,
			Name:     fmt.Sprintf("t%d", i),
			Magnet:   "magnet:?xt=urn:btih:" + hash,
		})
		if err != nil {
			t.Fatalf("seed queued: %v", err)
		}
	}
}

func countState(t *testing.T, st *store.Store, state string) int {
	t.Helper()
	n, err := st.CountByState(context.Background(), state)
	if err != nil {
		t.Fatalf("count %s: %v", state, err)
	}
	return n
}

func TestSubmitPassRespectsSlotLimit(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 5)

	id := 0
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		id++
		return okResult(id)
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)

	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if got := countState(t, st, store.StateTorBoxActive); got != 3 {
		t.Errorf("active = %d, want 3 (slot limit)", got)
	}
	if got := countState(t, st, store.StateQueued); got != 2 {
		t.Errorf("queued = %d, want 2", got)
	}
	if tb.calls != 3 {
		t.Errorf("createtorrent calls = %d, want 3", tb.calls)
	}
}

func TestSubmitPassCountsExistingActive(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 5)
	// Mark two as already active so only one slot is free.
	active := listQueued(t, st)
	if err := st.MarkActive(context.Background(), active[0].ID, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkActive(context.Background(), active[1].ID, 101); err != nil {
		t.Fatal(err)
	}

	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		return okResult(200)
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if tb.calls != 1 {
		t.Errorf("calls = %d, want 1 (only one free slot)", tb.calls)
	}
	if got := countState(t, st, store.StateTorBoxActive); got != 3 {
		t.Errorf("active = %d, want 3", got)
	}
}

func TestSubmitTooLargeErrors(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 1)
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		return nil, &torbox.APIError{StatusCode: 400, Code: "DOWNLOAD_TOO_LARGE", Detail: "too big"}
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if got := countState(t, st, store.StateError); got != 1 {
		t.Errorf("error = %d, want 1", got)
	}
}

func TestSubmitRateLimitStaysQueued(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 1)
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		return nil, &torbox.APIError{StatusCode: 429, Detail: "slow down"}
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if got := countState(t, st, store.StateQueued); got != 1 {
		t.Errorf("queued = %d, want 1 (rate limited stays queued)", got)
	}
	if got := countState(t, st, store.StateError); got != 0 {
		t.Errorf("error = %d, want 0", got)
	}
}

func TestSubmitTransientStaysQueued(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 1)
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		return nil, errors.New("connection refused")
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if got := countState(t, st, store.StateQueued); got != 1 {
		t.Errorf("queued = %d, want 1 (transient stays queued)", got)
	}
}

func TestSubmitPassesMagnet(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 1)
	var gotMagnet string
	tb := &fakeTB{fn: func(r torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		gotMagnet = r.Magnet
		return okResult(1)
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if gotMagnet == "" {
		t.Error("expected magnet passed to createtorrent")
	}
}

func listQueued(t *testing.T, st *store.Store) []store.Torrent {
	t.Helper()
	ts, err := st.ListByState(context.Background(), store.StateQueued)
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	return ts
}

// seedTorrentFull inserts a torrent with full state for DeleteTorrent tests.
func seedTorrentFull(t *testing.T, st *store.Store, hash, state, contentPath, savePath string, torboxID int) {
	t.Helper()
	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if torboxID > 0 {
		if err := st.MarkActive(ctx, tr.ID, torboxID); err != nil {
			t.Fatalf("mark active: %v", err)
		}
	}
	if state == store.StateComplete {
		if err := st.MarkComplete(ctx, tr.ID, contentPath, 100); err != nil {
			t.Fatalf("mark complete: %v", err)
		}
	} else if state == store.StateError {
		if err := st.MarkError(ctx, tr.ID, "test error"); err != nil {
			t.Fatalf("mark error: %v", err)
		}
	}
}

// touchFile creates a file with some content and returns its path.
// name may include path separators; intermediate directories are created.
func touchFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestDeleteTorrentRemovesAll(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	contentPath := filepath.Join(savePath, "show")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Put a local file on disk so we can verify it is removed.
	f := touchFile(t, contentPath, "e01.mkv")

	seedTorrentFull(t, st, hash, store.StateComplete, contentPath, savePath, 42)

	var ctlCalled int
	var ctlOp torbox.Operation
	tb := &fakeTB{ctl: func(_ int, op torbox.Operation) error {
		ctlCalled++
		ctlOp = op
		return nil
	}}
	e := New(st, tb, Config{MaxSlots: 1}, nil)

	if err := e.DeleteTorrent(context.Background(), hash, true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}

	// TorBox delete was called.
	if ctlCalled != 1 {
		t.Errorf("ControlTorrent called %d times, want 1", ctlCalled)
	}
	if ctlOp != torbox.OpDelete {
		t.Errorf("operation = %q, want delete", ctlOp)
	}

	// DB row is gone.
	if _, ok, _ := st.GetTorrent(context.Background(), hash); ok {
		t.Error("torrent still in DB after delete")
	}

	// Local file is gone.
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("local file not removed")
	}
}

func TestDeleteTorrentKeepsFilesWhenFalse(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	contentPath := filepath.Join(savePath, "show")
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	f := touchFile(t, contentPath, "e01.mkv")
	seedTorrentFull(t, st, hash, store.StateComplete, contentPath, savePath, 42)

	e := New(st, &fakeTB{}, Config{MaxSlots: 1}, nil)
	if err := e.DeleteTorrent(context.Background(), hash, false); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}

	// DB row is gone.
	if _, ok, _ := st.GetTorrent(context.Background(), hash); ok {
		t.Error("torrent still in DB after delete")
	}

	// Local file is kept.
	if _, err := os.Stat(f); err != nil {
		t.Errorf("local file should be kept: %v", err)
	}
}

func TestDeleteTorrentNoTorBoxID(t *testing.T) {
	// Torrent that never got a torbox_id (still QUEUED).
	st := newStore(t)
	hash := "cccccccccccccccccccccccccccccccccccccccc"
	seedTorrentFull(t, st, hash, store.StateQueued, "", "/downloads/tv", 0)

	var ctlCalled int
	tb := &fakeTB{ctl: func(_ int, _ torbox.Operation) error {
		ctlCalled++
		return nil
	}}
	e := New(st, tb, Config{MaxSlots: 1}, nil)

	if err := e.DeleteTorrent(context.Background(), hash, true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}

	// TorBox was never called (no torbox_id to delete).
	if ctlCalled != 0 {
		t.Errorf("ControlTorrent called %d times, want 0 (no torbox_id)", ctlCalled)
	}

	// DB row is gone.
	if _, ok, _ := st.GetTorrent(context.Background(), hash); ok {
		t.Error("torrent still in DB after delete")
	}
}

func TestDeleteTorrentNonExistent(t *testing.T) {
	st := newStore(t)
	e := New(st, &fakeTB{}, Config{MaxSlots: 1}, nil)

	if err := e.DeleteTorrent(context.Background(), "doesnotexist", true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	// Should be a silent no-op.
}

func TestDeleteTorrentTorBoxErrorStillCleansUp(t *testing.T) {
	st := newStore(t)
	hash := "dddddddddddddddddddddddddddddddddddddddd"
	savePath := filepath.Join(t.TempDir(), "tv")
	contentPath := filepath.Join(savePath, "show")
	f := touchFile(t, contentPath, "e01.mkv")
	seedTorrentFull(t, st, hash, store.StateComplete, contentPath, savePath, 99)

	tb := &fakeTB{ctl: func(_ int, _ torbox.Operation) error {
		return errors.New("torbox unreachable")
	}}
	e := New(st, tb, Config{MaxSlots: 1}, nil)

	if err := e.DeleteTorrent(context.Background(), hash, true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}

	// DB row is still gone (local cleanup is not gated on TorBox).
	if _, ok, _ := st.GetTorrent(context.Background(), hash); ok {
		t.Error("torrent still in DB after delete (should clean up despite TorBox error)")
	}

	// Local file is gone.
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("local file not removed")
	}
}

func TestDeleteTorrentRemovesStagingDir(t *testing.T) {
	st := newStore(t)
	tmp := t.TempDir()
	savePath := filepath.Join(tmp, "tv")
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	// Create a staging tree as if the downloader had been running.
	stagingDir := filepath.Join(savePath, ".incomplete", hash)
	stagingFile := touchFile(t, stagingDir, "show/e01.mkv.part")

	seedTorrentFull(t, st, hash, store.StateLocalDload, filepath.Join(savePath, "show"), savePath, 42)

	e := New(st, &fakeTB{}, Config{MaxSlots: 1, IncompleteSubdir: ".incomplete"}, nil)
	if err := e.DeleteTorrent(context.Background(), hash, true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}

	if _, err := os.Stat(stagingFile); !os.IsNotExist(err) {
		t.Error("staging file not removed")
	}
}

func TestDeleteTorrentCancelsActiveDownload(t *testing.T) {
	// Set up the engine with the torrent's download registered as active,
	// as downloadOne would do. The cancel should be called by DeleteTorrent.
	st := newStore(t)
	hash := "ffffffffffffffffffffffffffffffffffffffff"
	savePath := filepath.Join(t.TempDir(), "tv")
	seedTorrentFull(t, st, hash, store.StateLocalDload, filepath.Join(savePath, "show"), savePath, 42)

	var canceled bool
	e := New(st, &fakeTB{}, Config{MaxSlots: 1}, nil)
	e.mu.Lock()
	_, cancel := context.WithCancel(context.Background())
	e.activeDownload = &activeDownload{
		cancel:    func() { canceled = true; cancel() },
		infohash:  hash,
		torrentID: 1,
	}
	e.mu.Unlock()

	if err := e.DeleteTorrent(context.Background(), hash, true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}

	if !canceled {
		t.Error("active download was not cancelled by delete")
	}
}
