package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// seedActive adds one torrent, marks it TORBOX_ACTIVE with the given TorBox id,
// and backdates active_since by age so the fail-fast clock can be exercised.
func seedActive(t *testing.T, st *store.Store, infohash string, torboxID int, savePath string, age time.Duration) store.Torrent {
	t.Helper()
	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: infohash,
		Name:     "show",
		SavePath: savePath,
		Magnet:   "magnet:?xt=urn:btih:" + infohash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, torboxID); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	if age > 0 {
		since := time.Now().Add(-age).Unix()
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE torrents SET active_since = ? WHERE id = ?`, since, tr.ID); err != nil {
			t.Fatalf("backdate active_since: %v", err)
		}
	}
	got, _, err := st.GetTorrent(ctx, infohash)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

func getTorrent(t *testing.T, st *store.Store, infohash string) store.Torrent {
	t.Helper()
	tr, ok, err := st.GetTorrent(context.Background(), infohash)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", infohash, ok, err)
	}
	return tr
}

func TestReconcileMovesToLocalQueued(t *testing.T) {
	st := newStore(t)
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedActive(t, st, hash, 555, "/downloads/tv", 0)

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
				{ID: 1, Name: "show/show.S01E02.mkv", ShortName: "show.S01E02.mkv", Size: 200},
			},
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}

	tr := getTorrent(t, st, hash)
	if tr.State != store.StateLocalQueued {
		t.Fatalf("state = %q, want LOCAL_QUEUED", tr.State)
	}
	if tr.TorBoxProgress != 1 {
		t.Errorf("torbox_progress = %v, want 1", tr.TorBoxProgress)
	}
	want := filepath.Join("/downloads/tv", "show")
	if tr.ContentPath != want {
		t.Errorf("content_path = %q, want %q", tr.ContentPath, want)
	}
	files, err := st.ListFiles(context.Background(), tr.ID)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	if files[0].RelPath != "show/show.S01E01.mkv" || files[0].Size != 100 {
		t.Errorf("file[0] = %+v, want full path + size 100", files[0])
	}
}

func TestReconcileSingleFileContentPath(t *testing.T) {
	st := newStore(t)
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	seedActive(t, st, hash, 7, "/downloads/movies", 0)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID:              7,
			DownloadPresent: true,
			Files:           []torbox.TorrentFile{{ID: 0, Name: "movie.mkv", ShortName: "movie.mkv", Size: 42}},
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	tr := getTorrent(t, st, hash)
	want := filepath.Join("/downloads/movies", "movie.mkv")
	if tr.ContentPath != want {
		t.Errorf("content_path = %q, want %q", tr.ContentPath, want)
	}
}

func TestReconcileUpdatesProgress(t *testing.T) {
	st := newStore(t)
	hash := "cccccccccccccccccccccccccccccccccccccccc"
	seedActive(t, st, hash, 9, "/downloads/tv", time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID:            9,
			DownloadState: "downloading",
			Progress:      0.42,
			DownloadSpeed: 5000,
			Size:          1234,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	tr := getTorrent(t, st, hash)
	if tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE", tr.State)
	}
	if tr.TorBoxProgress != 0.42 {
		t.Errorf("torbox_progress = %v, want 0.42", tr.TorBoxProgress)
	}
	if tr.TorBoxState != "downloading" {
		t.Errorf("torbox_state = %q, want downloading", tr.TorBoxState)
	}
	if tr.DLSpeed != 5000 {
		t.Errorf("dlspeed = %d, want 5000", tr.DLSpeed)
	}
	if tr.Size != 1234 {
		t.Errorf("size = %d, want 1234", tr.Size)
	}
}

func TestReconcileFailFastTimeout(t *testing.T) {
	st := newStore(t)
	hash := "dddddddddddddddddddddddddddddddddddddddd"
	// Active for 25m, past the 20m default; still not present.
	seedActive(t, st, hash, 11, "/downloads/tv", 25*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 11, DownloadState: "stalled", DownloadPresent: false}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 20 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	tr := getTorrent(t, st, hash)
	if tr.State != store.StateError {
		t.Fatalf("state = %q, want ERROR", tr.State)
	}
	if tr.Error == "" {
		t.Error("expected an error reason")
	}
}

func TestReconcileNoFailBeforeTimeout(t *testing.T) {
	st := newStore(t)
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	seedActive(t, st, hash, 13, "/downloads/tv", 5*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 13, DownloadState: "downloading", Progress: 0.1}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 20 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE", tr.State)
	}
}

func TestReconcileFastFailDeadStall(t *testing.T) {
	st := newStore(t)
	hash := "3333333333333333333333333333333333333333"
	// Active 6m: past the 5m stall window but well under the 20m hard timeout.
	seedActive(t, st, hash, 21, "/downloads/tv", 6*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 21, DownloadState: "stalled", Seeds: 0, Peers: 0,
			Progress: 0, DownloadSpeed: 0, Cached: false, DownloadPresent: false,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 20 * time.Minute, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateError {
		t.Fatalf("state = %q, want ERROR (dead stall)", tr.State)
	}
}

func TestReconcileNoFastFailWithSeeds(t *testing.T) {
	st := newStore(t)
	hash := "4444444444444444444444444444444444444444"
	// Active 6m and not yet present, but TorBox found a seeder — keep the full
	// 20m window since it may still complete.
	seedActive(t, st, hash, 22, "/downloads/tv", 6*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 22, DownloadState: "stalled", Seeds: 1, Progress: 0, DownloadSpeed: 0,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 20 * time.Minute, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (has a seeder)", tr.State)
	}
}

func TestReconcileStallDisabled(t *testing.T) {
	st := newStore(t)
	hash := "5555555555555555555555555555555555555555"
	// Dead stall at 6m, but the early fast-fail is disabled (negative) → the
	// torrent keeps running until the 20m hard timeout.
	seedActive(t, st, hash, 23, "/downloads/tv", 6*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 23, DownloadState: "stalled", Seeds: 0}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 20 * time.Minute, StallTimeout: -1}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (stall fail disabled)", tr.State)
	}
}

func TestReconcileVanishedErrors(t *testing.T) {
	st := newStore(t)
	hash := "ffffffffffffffffffffffffffffffffffffffff"
	seedActive(t, st, hash, 99, "/downloads/tv", time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{}, nil // not on the account anymore
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateError {
		t.Errorf("state = %q, want ERROR (vanished)", tr.State)
	}
}

func TestReconcileVanishGrace(t *testing.T) {
	st := newStore(t)
	hash := "1111111111111111111111111111111111111111"
	// Just became active (age 0 < vanishGrace): a missing mylist entry is tolerated.
	seedActive(t, st, hash, 100, "/downloads/tv", 0)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (within vanish grace)", tr.State)
	}
}

func TestReconcileMyListErrorIsTransient(t *testing.T) {
	st := newStore(t)
	hash := "2222222222222222222222222222222222222222"
	seedActive(t, st, hash, 1, "/downloads/tv", time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return nil, context.DeadlineExceeded
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err == nil {
		t.Fatal("expected an error from a failing mylist")
	}
	// The torrent must stay active for a later retry, not be failed.
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE", tr.State)
	}
}
