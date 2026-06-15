package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	get   func(id int) (*torbox.Torrent, error)
	dl    func(p torbox.RequestDLParams) (string, error)
	ctl   func(torrentID int, op torbox.Operation) error
	ctlq  func(queuedID int, op torbox.Operation) error
	cache func(hashes []string) (map[string]torbox.CachedInfo, error)
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

func (f *fakeTB) GetTorrent(_ context.Context, id int, _ bool) (*torbox.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.get == nil {
		return nil, nil
	}
	return f.get(id)
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

func (f *fakeTB) ControlQueued(_ context.Context, queuedID int, op torbox.Operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctlq == nil {
		return nil
	}
	return f.ctlq(queuedID, op)
}

func (f *fakeTB) CheckCached(_ context.Context, hashes []string, _ bool) (map[string]torbox.CachedInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		return map[string]torbox.CachedInfo{}, nil
	}
	return f.cache(hashes)
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

// seedQueued2 adds n more QUEUED torrents with hashes that don't collide with
// seedQueued's (offset into a different range).
func seedQueued2(t *testing.T, st *store.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("%040x", i+1000)
		_, _, err := st.AddTorrent(context.Background(), store.AddTorrentParams{
			Infohash: hash,
			Name:     fmt.Sprintf("w%d", i),
			Magnet:   "magnet:?xt=urn:btih:" + hash,
		})
		if err != nil {
			t.Fatalf("seed queued2: %v", err)
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

// countingHandler records how many log records carry a given message.
type countingHandler struct {
	mu   sync.Mutex
	msgs map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs[r.Message]++
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.msgs[msg]
}

func TestSubmitPassLogsSlotWaitOnce(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 2)
	// Occupy both slots so no submission is possible.
	q := listQueued(t, st)
	if err := st.MarkActive(context.Background(), q[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkActive(context.Background(), q[1].ID, 2); err != nil {
		t.Fatal(err)
	}
	// Two more torrents waiting on a slot.
	seedQueued2(t, st, 2)

	h := &countingHandler{msgs: map[string]int{}}
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		return okResult(99)
	}}
	e := New(st, tb, Config{MaxSlots: 2}, slog.New(h))

	const waitMsg = "all TorBox slots busy; queued torrents waiting for a slot"
	const resumeMsg = "TorBox slot freed; resuming submissions"

	// Several passes while slots stay full → logged exactly once.
	for i := 0; i < 3; i++ {
		if err := e.submitPass(context.Background()); err != nil {
			t.Fatalf("submitPass: %v", err)
		}
	}
	if got := h.count(waitMsg); got != 1 {
		t.Errorf("slot-wait logged %d times, want 1", got)
	}

	// Free a slot; the next pass should resume and submit.
	active, err := st.ListByState(context.Background(), store.StateTorBoxActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkError(context.Background(), active[0].ID, "freed for test"); err != nil {
		t.Fatal(err)
	}
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if got := h.count(resumeMsg); got != 1 {
		t.Errorf("resume logged %d times, want 1", got)
	}
	if tb.calls == 0 {
		t.Error("expected a submission after a slot freed")
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

func TestSubmitRateLimitCooldown(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 2)
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		return nil, &torbox.APIError{StatusCode: 429, Detail: "slow down"}
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)

	// First pass hits 429 on the first torrent and enters cooldown; it must not try
	// the second torrent in the same pass.
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if tb.calls != 1 {
		t.Fatalf("calls after first pass = %d, want 1 (cooldown stops further tries)", tb.calls)
	}
	// A subsequent pass during cooldown makes no createtorrent calls at all.
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if tb.calls != 1 {
		t.Errorf("calls after second pass = %d, want still 1 (in cooldown)", tb.calls)
	}
	if got := countState(t, st, store.StateQueued); got != 2 {
		t.Errorf("queued = %d, want 2 (both stay queued)", got)
	}
}

func TestSubmitQueuedResultMarksQueued(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 1)
	qid := 555
	tb := &fakeTB{fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
		// TorBox queued it: queued_id only, no torrent_id.
		return &torbox.CreateTorrentResult{QueuedID: &qid}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	active, err := st.ListByState(context.Background(), store.StateTorBoxActive)
	if err != nil || len(active) != 1 {
		t.Fatalf("list active: err=%v n=%d", err, len(active))
	}
	tr := active[0]
	if tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want TORBOX_ACTIVE", tr.State)
	}
	if tr.TorBoxState != "queued" {
		t.Errorf("torbox_state = %q, want queued", tr.TorBoxState)
	}
	// The queued_id is tracked in its own column, NOT torbox_id (a different
	// namespace): controltorrent can't delete a queued download.
	if !tr.TorBoxQueuedID.Valid || tr.TorBoxQueuedID.Int64 != int64(qid) {
		t.Errorf("torbox_queued_id = %v, want %d", tr.TorBoxQueuedID, qid)
	}
	if tr.TorBoxID.Valid {
		t.Errorf("torbox_id = %v, want NULL for a queued download", tr.TorBoxID)
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

func TestSubmitCacheCheckIsInformationalOnly(t *testing.T) {
	// With cache_check enabled and the release reported as NOT cached, submission
	// must still proceed — the cache check only logs, it never gates.
	st := newStore(t)
	seedQueued(t, st, 1)

	var cacheCalls int
	tb := &fakeTB{
		fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
			return okResult(1)
		},
		cache: func([]string) (map[string]torbox.CachedInfo, error) {
			cacheCalls++
			return map[string]torbox.CachedInfo{}, nil // not cached
		},
	}
	e := New(st, tb, Config{MaxSlots: 3, CacheCheck: true}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if cacheCalls != 1 {
		t.Errorf("CheckCached calls = %d, want 1", cacheCalls)
	}
	if got := countState(t, st, store.StateTorBoxActive); got != 1 {
		t.Errorf("active = %d, want 1 (uncached release still submitted)", got)
	}
	// The (not-)cached result is recorded for the dashboard.
	tr, _, err := st.GetTorrent(context.Background(), fmt.Sprintf("%040x", 1))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tr.Cached {
		t.Errorf("cached = true, want false (release was not cached)")
	}
}

func TestSubmitRecordsCacheHit(t *testing.T) {
	// A release already on TorBox is flagged cached so the dashboard can show an
	// instant hit.
	st := newStore(t)
	seedQueued(t, st, 1)

	tb := &fakeTB{
		fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
			return okResult(1)
		},
		cache: func([]string) (map[string]torbox.CachedInfo, error) {
			return map[string]torbox.CachedInfo{"x": {}}, nil // cached
		},
	}
	e := New(st, tb, Config{MaxSlots: 3, CacheCheck: true}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	tr, _, err := st.GetTorrent(context.Background(), fmt.Sprintf("%040x", 1))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !tr.Cached {
		t.Errorf("cached = false, want true (release was cached on TorBox)")
	}
}

func TestSubmitNoCacheCheckWhenDisabled(t *testing.T) {
	st := newStore(t)
	seedQueued(t, st, 1)

	var cacheCalls int
	tb := &fakeTB{
		fn: func(torbox.CreateTorrentRequest) (*torbox.CreateTorrentResult, error) {
			return okResult(1)
		},
		cache: func([]string) (map[string]torbox.CachedInfo, error) {
			cacheCalls++
			return nil, nil
		},
	}
	e := New(st, tb, Config{MaxSlots: 3, CacheCheck: false}, nil)
	if err := e.submitPass(context.Background()); err != nil {
		t.Fatalf("submitPass: %v", err)
	}
	if cacheCalls != 0 {
		t.Errorf("CheckCached calls = %d, want 0 (cache_check disabled)", cacheCalls)
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

func TestDeleteTorrentQueuedUsesControlQueued(t *testing.T) {
	// A torrent still parked in TorBox's queue must be removed via controlqueued
	// (queued_id), not controltorrent — its queued_id is a separate namespace.
	st := newStore(t)
	ctx := context.Background()
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{Infohash: hash, Name: "show"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkQueued(ctx, tr.ID, 7777); err != nil {
		t.Fatalf("mark queued: %v", err)
	}

	var ctlCalled, ctlqCalled, gotQueuedID int
	var gotOp torbox.Operation
	tb := &fakeTB{
		ctl:  func(int, torbox.Operation) error { ctlCalled++; return nil },
		ctlq: func(id int, op torbox.Operation) error { ctlqCalled++; gotQueuedID = id; gotOp = op; return nil },
	}
	e := New(st, tb, Config{MaxSlots: 1}, nil)

	if err := e.DeleteTorrent(ctx, hash, true); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	if ctlCalled != 0 {
		t.Errorf("ControlTorrent called %d times, want 0 (queued, not active)", ctlCalled)
	}
	if ctlqCalled != 1 || gotQueuedID != 7777 || gotOp != torbox.OpDelete {
		t.Errorf("ControlQueued calls=%d id=%d op=%q, want 1 delete on id 7777", ctlqCalled, gotQueuedID, gotOp)
	}
	if _, ok, _ := st.GetTorrent(ctx, hash); ok {
		t.Error("torrent still in DB after delete")
	}
}

func TestFailQueuedUsesControlQueued(t *testing.T) {
	// When a still-queued torrent is failed, cleanup targets controlqueued so the
	// queued download is actually removed (the bug that orphaned 62 downloads).
	st := newStore(t)
	ctx := context.Background()
	hash := "ffffffffffffffffffffffffffffffffffffffff"
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{Infohash: hash, Name: "show"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkQueued(ctx, tr.ID, 8888); err != nil {
		t.Fatalf("mark queued: %v", err)
	}
	tr, _, err = st.GetTorrent(ctx, hash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var ctlCalled, ctlqCalled, gotQueuedID int
	tb := &fakeTB{
		ctl:  func(int, torbox.Operation) error { ctlCalled++; return nil },
		ctlq: func(id int, _ torbox.Operation) error { ctlqCalled++; gotQueuedID = id; return nil },
	}
	e := New(st, tb, Config{MaxSlots: 1}, nil)

	e.fail(ctx, tr, "stalled in queue")

	if ctlCalled != 0 {
		t.Errorf("ControlTorrent called %d times, want 0 (queued, not active)", ctlCalled)
	}
	if ctlqCalled != 1 || gotQueuedID != 8888 {
		t.Errorf("ControlQueued calls=%d id=%d, want 1 on id 8888", ctlqCalled, gotQueuedID)
	}
	if got := countState(t, st, store.StateError); got != 1 {
		t.Errorf("error state = %d, want 1", got)
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
