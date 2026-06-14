package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func newRebaseStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func hash(n int) string { return fmt.Sprintf("%040x", n) }

func TestRebaseToRoot(t *testing.T) {
	ctx := context.Background()
	st := newRebaseStore(t)

	// Stale category from a previous root.
	if err := st.UpsertCategory(ctx, "tv-sonarr", "/downloads/tv-sonarr"); err != nil {
		t.Fatal(err)
	}

	// A pending (QUEUED) torrent with the old save path — should move.
	pending, _, err := st.AddTorrent(ctx, AddTorrentParams{
		Infohash: hash(1), Name: "show", Category: "tv-sonarr", SavePath: "/downloads/tv-sonarr",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A completed torrent at the old path — must NOT move (files already on disk).
	done, _, err := st.AddTorrent(ctx, AddTorrentParams{
		Infohash: hash(2), Name: "movie", Category: "radarr", SavePath: "/downloads/radarr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkComplete(ctx, done.ID, "/downloads/radarr/movie.mkv", 100); err != nil {
		t.Fatal(err)
	}

	cats, tors, err := st.RebaseToRoot(ctx, "/data/downloads")
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if cats != 1 {
		t.Errorf("categories changed = %d, want 1", cats)
	}
	if tors != 1 {
		t.Errorf("torrents changed = %d, want 1 (only the pending one)", tors)
	}

	// Category now derives from the new root.
	if c, ok, _ := st.GetCategory(ctx, "tv-sonarr"); !ok || c.SavePath != "/data/downloads/tv-sonarr" {
		t.Errorf("category save path = %q, want /data/downloads/tv-sonarr", c.SavePath)
	}

	// Pending torrent rebased.
	if got, _, _ := st.GetTorrent(ctx, pending.Infohash); got.SavePath != "/data/downloads/tv-sonarr" {
		t.Errorf("pending save path = %q, want /data/downloads/tv-sonarr", got.SavePath)
	}

	// Completed torrent untouched.
	if got, _, _ := st.GetTorrent(ctx, done.Infohash); got.SavePath != "/downloads/radarr" {
		t.Errorf("completed save path = %q, want /downloads/radarr (unchanged)", got.SavePath)
	}
}

func TestRebaseToRootUncategorized(t *testing.T) {
	ctx := context.Background()
	st := newRebaseStore(t)

	// No category: save path should become the bare root.
	tr, _, err := st.AddTorrent(ctx, AddTorrentParams{
		Infohash: hash(1), Name: "loose", SavePath: "/downloads",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, tors, err := st.RebaseToRoot(ctx, "/data/downloads")
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if tors != 1 {
		t.Fatalf("torrents changed = %d, want 1", tors)
	}
	if got, _, _ := st.GetTorrent(ctx, tr.Infohash); got.SavePath != "/data/downloads" {
		t.Errorf("save path = %q, want /data/downloads", got.SavePath)
	}
}

func TestRebaseToRootNoop(t *testing.T) {
	ctx := context.Background()
	st := newRebaseStore(t)

	if err := st.UpsertCategory(ctx, "tv-sonarr", "/data/downloads/tv-sonarr"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddTorrent(ctx, AddTorrentParams{
		Infohash: hash(1), Category: "tv-sonarr", SavePath: "/data/downloads/tv-sonarr",
	}); err != nil {
		t.Fatal(err)
	}

	cats, tors, err := st.RebaseToRoot(ctx, "/data/downloads")
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if cats != 0 || tors != 0 {
		t.Errorf("changed cats=%d tors=%d, want 0/0 (already on root)", cats, tors)
	}
}
