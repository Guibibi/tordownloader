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

// MarkQueued records the queued_id in its own column and leaves torbox_id NULL,
// so cleanup can tell a queued download (needs controlqueued) from an active
// torrent (needs controltorrent).
func TestMarkQueuedTracksQueuedIDSeparately(t *testing.T) {
	ctx := context.Background()
	st := newWritesStore(t)

	tr, _, err := st.AddTorrent(ctx, AddTorrentParams{Infohash: hash(1), Name: "show"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkQueued(ctx, tr.ID, 4321); err != nil {
		t.Fatalf("mark queued: %v", err)
	}

	got, ok, err := st.GetTorrent(ctx, tr.Infohash)
	if err != nil || !ok {
		t.Fatalf("get: err=%v ok=%v", err, ok)
	}
	if got.State != StateTorBoxActive {
		t.Errorf("state = %q, want %q", got.State, StateTorBoxActive)
	}
	if got.TorBoxState != "queued" {
		t.Errorf("torbox_state = %q, want queued", got.TorBoxState)
	}
	if !got.TorBoxQueuedID.Valid || got.TorBoxQueuedID.Int64 != 4321 {
		t.Errorf("torbox_queued_id = %v, want 4321", got.TorBoxQueuedID)
	}
	if got.TorBoxID.Valid {
		t.Errorf("torbox_id = %v, want NULL", got.TorBoxID)
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
	if err := st.MarkQueued(ctx, tr.ID, 4321); err != nil {
		t.Fatalf("mark queued: %v", err)
	}
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
