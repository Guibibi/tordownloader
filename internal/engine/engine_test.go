package engine

import (
	"context"
	"errors"
	"fmt"
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
