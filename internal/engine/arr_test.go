package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
)

// fakeArr is a scriptable ArrNotifier.
type fakeArr struct {
	mu    sync.Mutex
	calls []string // infohashes notified, in order
	fn    func(infohash string) (bool, error)
}

func (f *fakeArr) NotifyFailed(_ context.Context, infohash, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, infohash)
	if f.fn == nil {
		return true, nil
	}
	return f.fn(infohash)
}

// seedError adds one torrent and moves it to ERROR, returning its row.
func seedError(t *testing.T, st *store.Store, hash string) store.Torrent {
	t.Helper()
	tor, _, err := st.AddTorrent(context.Background(), store.AddTorrentParams{
		Infohash: hash, Name: "failed-" + hash[:6], Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.MarkError(context.Background(), tor.ID, "no download progress"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	return tor
}

func TestArrPassNotifiesAndMarks(t *testing.T) {
	st := newStore(t)
	tor := seedError(t, st, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	fa := &fakeArr{}
	e := New(st, &fakeTB{}, Config{MaxSlots: 1, Arr: fa}, nil)
	e.arrPass(context.Background())

	if len(fa.calls) != 1 || fa.calls[0] != tor.Infohash {
		t.Fatalf("expected one notify for %s, got %v", tor.Infohash, fa.calls)
	}
	pending, err := st.ListArrUnnotified(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected row marked notified, still pending: %d", len(pending))
	}
}

func TestArrPassNilNotifierNoop(t *testing.T) {
	st := newStore(t)
	seedError(t, st, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	e := New(st, &fakeTB{}, Config{MaxSlots: 1}, nil)
	e.arrPass(context.Background()) // must not panic or mark anything
	pending, _ := st.ListArrUnnotified(context.Background())
	if len(pending) != 1 {
		t.Fatalf("expected row still pending with no notifier, got %d", len(pending))
	}
}

func TestArrPassRetriesErrorsThrottled(t *testing.T) {
	st := newStore(t)
	seedError(t, st, "cccccccccccccccccccccccccccccccccccccccc")

	fa := &fakeArr{fn: func(string) (bool, error) { return false, errors.New("sonarr down") }}
	e := New(st, &fakeTB{}, Config{MaxSlots: 1, Arr: fa}, nil)

	e.arrPass(context.Background())
	e.arrPass(context.Background()) // within arrNotifyInterval: throttled
	if len(fa.calls) != 1 {
		t.Fatalf("expected 1 call (second throttled), got %d", len(fa.calls))
	}
	pending, _ := st.ListArrUnnotified(context.Background())
	if len(pending) != 1 {
		t.Fatalf("row must stay pending while the *arr is unreachable, got %d pending", len(pending))
	}

	// Age the attempt past the interval: it retries, and on success marks done.
	fa.fn = nil
	for id, at := range e.arrAttempts {
		at.last = at.last.Add(-2 * arrNotifyInterval)
		e.arrAttempts[id] = at
	}
	e.arrPass(context.Background())
	if len(fa.calls) != 2 {
		t.Fatalf("expected retry after interval, got %d calls", len(fa.calls))
	}
	pending, _ = st.ListArrUnnotified(context.Background())
	if len(pending) != 0 {
		t.Fatalf("expected row notified after successful retry, got %d pending", len(pending))
	}
}

func TestArrPassGivesUpOnNotFoundAfterGrace(t *testing.T) {
	st := newStore(t)
	seedError(t, st, "dddddddddddddddddddddddddddddddddddddddd")

	fa := &fakeArr{fn: func(string) (bool, error) { return false, nil }} // clean not-found
	e := New(st, &fakeTB{}, Config{MaxSlots: 1, Arr: fa}, nil)

	e.arrPass(context.Background())
	pending, _ := st.ListArrUnnotified(context.Background())
	if len(pending) != 1 {
		t.Fatalf("a fresh not-found must keep retrying (queue refresh lag), got %d pending", len(pending))
	}

	// Age the first attempt past the grace: the next pass gives up and marks it.
	for id, at := range e.arrAttempts {
		at.first = at.first.Add(-arrNotFoundGrace - time.Minute)
		at.last = at.last.Add(-2 * arrNotifyInterval)
		e.arrAttempts[id] = at
	}
	e.arrPass(context.Background())
	pending, _ = st.ListArrUnnotified(context.Background())
	if len(pending) != 0 {
		t.Fatalf("expected give-up after grace, got %d pending", len(pending))
	}
	if len(e.arrAttempts) != 0 {
		t.Fatalf("expected attempt bookkeeping pruned, got %d entries", len(e.arrAttempts))
	}
}
