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

// CountActiveSlots counts only torrents TorBox is actively caching (state
// TORBOX_ACTIVE) — the real concurrent-slot occupancy. A QUEUED torrent isn't
// submitted yet, and one we've moved to LOCAL_* (cached, pulling from the CDN)
// is inactive on TorBox; neither holds a slot, even while carrying a torbox_id.
func TestCountActiveSlots(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	mk := func(h string) int64 {
		tr, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: h, Name: "show"})
		if err != nil {
			t.Fatal(err)
		}
		return tr.ID
	}
	a := mk(hash(1))       // caching on TorBox → counts
	b := mk(hash(2))       // caching on TorBox → counts
	pending := mk(hash(3)) // QUEUED, not submitted → no slot
	_ = pending

	if err := st.MarkActive(ctx, a, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkActive(ctx, b, 200); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountActiveSlots(ctx); err != nil || n != 2 {
		t.Fatalf("CountActiveSlots = %d (err %v), want 2", n, err)
	}

	// Once TorBox has cached it, we move it to LOCAL_QUEUED to pull from the CDN;
	// it goes inactive on TorBox and frees its slot, even though it keeps its
	// torbox_id until we later delete it.
	if err := st.MarkLocalQueued(ctx, a, "/downloads/show", 10, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountActiveSlots(ctx); err != nil || n != 1 {
		t.Fatalf("CountActiveSlots after caching = %d (err %v), want 1", n, err)
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

// RequeueFailed resets only ERROR rows, clearing everything the failed attempt
// left behind and taking the new add's source and category.
func TestRequeueFailed(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	failed, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: "aa00000000000000000000000000000000000000", Name: "old", Category: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkActive(ctx, failed.ID, 5); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkError(ctx, failed.ID, "no download progress"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkArrNotified(ctx, failed.ID); err != nil {
		t.Fatal(err)
	}

	ok, err := st.RequeueFailed(ctx, AddTorrentParams{
		Infohash: failed.Infohash, Name: "new", Category: "tv-4k", SavePath: "/downloads/tv-4k", Magnet: "magnet:?xt=urn:btih:x",
	})
	if err != nil || !ok {
		t.Fatalf("RequeueFailed = %v, %v; want true", ok, err)
	}
	got, _, _ := st.GetTorrent(ctx, failed.Infohash)
	switch {
	case got.State != StateQueued:
		t.Errorf("state = %q, want QUEUED", got.State)
	case got.Error != "" || got.ArrNotified:
		t.Errorf("error = %q arr_notified = %v, want both cleared", got.Error, got.ArrNotified)
	case got.TorBoxID.Valid || got.ActiveSince != 0:
		t.Errorf("torbox_id = %v active_since = %d, want cleared", got.TorBoxID, got.ActiveSince)
	case got.Name != "new" || got.Category != "tv-4k" || got.SavePath != "/downloads/tv-4k":
		t.Errorf("name/category/save_path = %q/%q/%q, want the new add's", got.Name, got.Category, got.SavePath)
	}
	if magnet, _, _ := st.Source(ctx, got.ID); magnet != "magnet:?xt=urn:btih:x" {
		t.Errorf("magnet = %q, want the new add's", magnet)
	}

	// A torrent that isn't in ERROR is left alone.
	ok, err = st.RequeueFailed(ctx, AddTorrentParams{Infohash: failed.Infohash, Name: "again"})
	if err != nil || ok {
		t.Errorf("RequeueFailed on a QUEUED row = %v, %v; want false", ok, err)
	}
}

// CountStates reports every known state, including empty ones, so metric
// series don't disappear when a count drops to zero.
func TestCountStates(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)
	for _, h := range []string{"aa", "bb"} {
		if _, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: h, Name: "show"}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	tr, _, _ := st.GetTorrent(ctx, "bb")
	if err := st.MarkError(ctx, tr.ID, "boom"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	got, err := st.CountStates(ctx)
	if err != nil {
		t.Fatalf("CountStates: %v", err)
	}
	if got[StateQueued] != 1 || got[StateError] != 1 {
		t.Errorf("counts = %v, want 1 QUEUED and 1 ERROR", got)
	}
	if n, ok := got[StateComplete]; !ok || n != 0 {
		t.Errorf("COMPLETE = (%d, %v), want present with 0", n, ok)
	}
}
