package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newWritesStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// setQueuedID parks a torrent in the legacy TorBox-queued shape (TORBOX_ACTIVE
// with only a queued_id) directly, since the engine no longer produces it.
func setQueuedID(t *testing.T, st *Store, id int64, queuedID int) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE torrents SET state = ?, torbox_queued_id = ? WHERE id = ?`,
		StateTorBoxActive, queuedID, id); err != nil {
		t.Fatalf("set queued id: %v", err)
	}
}

// CountOnTorBox counts every torrent still holding a TorBox identifier (active id
// or queued id) — the true slot occupancy — and excludes rows whose refs were
// cleared on deletion.
func TestCountOnTorBox(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	mk := func(h string) int64 {
		tr, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: h, Name: "show"})
		if err != nil {
			t.Fatal(err)
		}
		return tr.ID
	}
	active := mk(hash(1))  // active torrent id → counts
	queued := mk(hash(2))  // legacy queued id → counts
	pending := mk(hash(3)) // QUEUED, no refs → does not count
	_ = pending

	if err := st.MarkActive(ctx, active, 100); err != nil {
		t.Fatal(err)
	}
	setQueuedID(t, st, queued, 200)

	if n, err := st.CountOnTorBox(ctx); err != nil || n != 2 {
		t.Fatalf("CountOnTorBox = %d (err %v), want 2", n, err)
	}

	// Clearing one's refs frees its slot from the count.
	if err := st.ClearTorBoxRefs(ctx, active); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountOnTorBox(ctx); err != nil || n != 1 {
		t.Fatalf("CountOnTorBox after clear = %d (err %v), want 1", n, err)
	}
}

// ListReapable returns only COMPLETE/ERROR torrents that still carry a TorBox ref
// — finished work that is still pinning a slot. Active torrents and already-
// cleared rows are excluded.
func TestListReapable(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	mk := func(h string) int64 {
		tr, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: h, Name: "show"})
		if err != nil {
			t.Fatal(err)
		}
		return tr.ID
	}

	// COMPLETE + torbox_id → reapable.
	done := mk(hash(1))
	if err := st.MarkActive(ctx, done, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkComplete(ctx, done, "/downloads/show", 10); err != nil {
		t.Fatal(err)
	}
	// ERROR + torbox_id → reapable.
	errd := mk(hash(2))
	if err := st.MarkActive(ctx, errd, 200); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkError(ctx, errd, "boom"); err != nil {
		t.Fatal(err)
	}
	// COMPLETE but refs already cleared → not reapable.
	clean := mk(hash(3))
	if err := st.MarkActive(ctx, clean, 300); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkComplete(ctx, clean, "/downloads/x", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearTorBoxRefs(ctx, clean); err != nil {
		t.Fatal(err)
	}
	// Active torrent (not finished) → not reapable.
	live := mk(hash(4))
	if err := st.MarkActive(ctx, live, 400); err != nil {
		t.Fatal(err)
	}

	reap, err := st.ListReapable(ctx)
	if err != nil {
		t.Fatalf("ListReapable: %v", err)
	}
	if len(reap) != 2 {
		t.Fatalf("reapable count = %d, want 2 (the completed and errored, still on TorBox)", len(reap))
	}
	got := map[int64]bool{}
	for _, r := range reap {
		got[r.ID] = true
	}
	if !got[done] || !got[errd] {
		t.Errorf("reapable = %v, want the completed and errored rows", got)
	}
}

// When a queued download activates under a real torrent id, adopting that id
// clears the queued_id: the queue entry is consumed, so later cleanup targets the
// torrent id (controltorrent), not the stale queued_id.
func TestUpdateTorBoxIDClearsQueuedID(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	tr, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: hash(2), Name: "show"})
	if err != nil {
		t.Fatal(err)
	}
	setQueuedID(t, st, tr.ID, 4321)
	if err := st.UpdateTorBoxID(ctx, tr.ID, 9001); err != nil {
		t.Fatalf("update torbox id: %v", err)
	}

	got, _, err := st.GetTorrent(ctx, tr.Infohash)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TorBoxID.Valid || got.TorBoxID.Int64 != 9001 {
		t.Errorf("torbox_id = %v, want 9001", got.TorBoxID)
	}
	if got.TorBoxQueuedID.Valid {
		t.Errorf("torbox_queued_id = %v, want NULL after activation", got.TorBoxQueuedID)
	}
}
