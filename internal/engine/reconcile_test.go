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
		// Move both clocks back together: a torrent active `age` ago that has not
		// progressed since (tests override progress_at separately when they need a
		// torrent that became active long ago but advanced recently).
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE torrents SET active_since = ?, progress_at = ? WHERE id = ?`, since, since, tr.ID); err != nil {
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
			Seeds:         14,
			Peers:         3,
			ETA:           600,
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
	if tr.Seeds != 14 || tr.Peers != 3 {
		t.Errorf("seeds/peers = %d/%d, want 14/3", tr.Seeds, tr.Peers)
	}
	if tr.ETA != 600 {
		t.Errorf("eta = %d, want 600", tr.ETA)
	}
	if tr.Size != 1234 {
		t.Errorf("size = %d, want 1234", tr.Size)
	}
}

func TestReconcileAbsoluteCap(t *testing.T) {
	st := newStore(t)
	hash := "dddddddddddddddddddddddddddddddddddddddd"
	// Active 25m, past the opt-in 20m cap; stall detection disabled so this
	// exercises the cap alone.
	seedActive(t, st, hash, 11, "/downloads/tv", 25*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 11, DownloadState: "downloading", DownloadSpeed: 1, DownloadPresent: false}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 20 * time.Minute, StallTimeout: -1}, nil)
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

func TestReconcileNoCapByDefault(t *testing.T) {
	st := newStore(t)
	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	// Active 30m but downloading slowly: with the cap disabled (FailTimeout 0) and
	// bytes still moving, it must not be failed for being slow.
	seedActive(t, st, hash, 13, "/downloads/tv", 30*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 13, DownloadState: "downloading", Progress: 0.1, DownloadSpeed: 12500}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 0, StallTimeout: 10 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (slow but moving)", tr.State)
	}
}

func TestReconcileStallFail(t *testing.T) {
	st := newStore(t)
	hash := "3333333333333333333333333333333333333333"
	// No bytes and no progress for 6m, past the 5m stall window → fail so Sonarr
	// blacklists the release.
	seedActive(t, st, hash, 21, "/downloads/tv", 6*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 21, DownloadState: "stalled", Active: true, Seeds: 0, Peers: 0,
			Progress: 0, DownloadSpeed: 0, Cached: false, DownloadPresent: false,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateError {
		t.Fatalf("state = %q, want ERROR (stalled)", tr.State)
	}
}

func TestReconcileCachedUsesLongerStallWindow(t *testing.T) {
	st := newStore(t)
	hash := "9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"
	// Cached release sitting at 0% for 8m: past the 5m normal stall window, but
	// within the 30m cached grace, so it must NOT be failed — it's just waiting for
	// TorBox to surface bytes it already has.
	tr := seedActive(t, st, hash, 41, "/downloads/tv", 8*time.Minute)
	if err := st.SetTorBoxCached(context.Background(), tr.ID, true); err != nil {
		t.Fatalf("set cached: %v", err)
	}

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 41, DownloadState: "downloading", Active: true,
			Progress: 0, DownloadSpeed: 0, Cached: true, DownloadPresent: false,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute, CachedStallTimeout: 30 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Fatalf("state = %q, want TORBOX_ACTIVE (cached, within longer window)", tr.State)
	}
}

func TestReconcileCachedStallFailPastCachedWindow(t *testing.T) {
	st := newStore(t)
	hash := "9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b"
	// Cached release stuck at 0% for 31m, past even the 30m cached grace: the
	// hand-off is genuinely broken, so the safety net fails it.
	tr := seedActive(t, st, hash, 42, "/downloads/tv", 31*time.Minute)
	if err := st.SetTorBoxCached(context.Background(), tr.ID, true); err != nil {
		t.Fatalf("set cached: %v", err)
	}

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 42, DownloadState: "downloading", Active: true,
			Progress: 0, DownloadSpeed: 0, Cached: true, DownloadPresent: false,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute, CachedStallTimeout: 30 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateError {
		t.Fatalf("state = %q, want ERROR (cached but broken past 30m)", tr.State)
	}
}

func TestReconcileStallFailDeletesFromTorBox(t *testing.T) {
	st := newStore(t)
	hash := "8888888888888888888888888888888888888888"
	seedActive(t, st, hash, 31, "/downloads/tv", 6*time.Minute)

	var gotID int
	var gotOp torbox.Operation
	calls := 0
	tb := &fakeTB{
		list: func() ([]torbox.Torrent, error) {
			return []torbox.Torrent{{ID: 31, DownloadState: "stalled", Active: true, Seeds: 0, Progress: 0, DownloadSpeed: 0}}, nil
		},
		ctl: func(id int, op torbox.Operation) error {
			calls++
			gotID, gotOp = id, op
			return nil
		},
	}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateError {
		t.Fatalf("state = %q, want ERROR", tr.State)
	}
	if calls != 1 || gotID != 31 || gotOp != torbox.OpDelete {
		t.Errorf("ControlTorrent calls=%d id=%d op=%q, want 1 delete on id 31", calls, gotID, gotOp)
	}
}

func TestReconcileStallFailEvenWithSeeds(t *testing.T) {
	st := newStore(t)
	hash := "4444444444444444444444444444444444444444"
	// Connected to a seeder but moving no bytes and not climbing for 6m: "not
	// going up at all" is a stall regardless of seed count → fail.
	seedActive(t, st, hash, 22, "/downloads/tv", 6*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 22, DownloadState: "stalled", Active: true, Seeds: 1, Progress: 0, DownloadSpeed: 0,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateError {
		t.Errorf("state = %q, want ERROR (stalled despite a seeder)", tr.State)
	}
}

func TestReconcileNoStallWhileMovingBytes(t *testing.T) {
	st := newStore(t)
	hash := "6666666666666666666666666666666666666666"
	// Active well past the stall window but TorBox is actively pulling bytes
	// (100kbps ≈ 12500 B/s) with progress not yet ticking up — must not fail.
	seedActive(t, st, hash, 24, "/downloads/tv", 30*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{
			ID: 24, DownloadState: "downloading", Seeds: 2, Progress: 0, DownloadSpeed: 12500,
		}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (bytes moving)", tr.State)
	}
}

func TestReconcileStallClockResetByProgress(t *testing.T) {
	st := newStore(t)
	hash := "7777777777777777777777777777777777777777"
	// Became active 30m ago but advanced just 1m ago: the stall clock runs from
	// the last progress, so a brief current stall (0 bytes now) must not fail it.
	tr := seedActive(t, st, hash, 25, "/downloads/tv", 30*time.Minute)
	recent := time.Now().Add(-1 * time.Minute).Unix()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE torrents SET progress_at = ?, torbox_progress = 0.4 WHERE id = ?`, recent, tr.ID); err != nil {
		t.Fatalf("set progress_at: %v", err)
	}

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 25, DownloadState: "stalled", Progress: 0.4, DownloadSpeed: 0}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, StallTimeout: 5 * time.Minute}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (progressed recently)", tr.State)
	}
}

func TestReconcileStallDisabled(t *testing.T) {
	st := newStore(t)
	hash := "5555555555555555555555555555555555555555"
	// Dead stall at 6m, but stall detection is disabled (negative) and there's no
	// absolute cap → the torrent keeps running.
	seedActive(t, st, hash, 23, "/downloads/tv", 6*time.Minute)

	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 23, DownloadState: "stalled", Active: true, Seeds: 0}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3, FailTimeout: 0, StallTimeout: -1}, nil)
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
	// Past the vanish grace, absent from mylist, and a direct lookup (default fake:
	// nil) confirms it's gone → fail.
	seedActive(t, st, hash, 99, "/downloads/tv", 3*time.Minute)

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

func TestReconcileConfirmAvoidsVanish(t *testing.T) {
	st := newStore(t)
	hash := "abababababababababababababababababababab"
	// Absent from mylist and past grace, but a direct lookup still finds it (mylist
	// was just lagging/paging) → must not be failed.
	seedActive(t, st, hash, 42, "/downloads/tv", 3*time.Minute)

	tb := &fakeTB{
		list: func() ([]torbox.Torrent, error) { return []torbox.Torrent{}, nil },
		get:  func(id int) (*torbox.Torrent, error) { return &torbox.Torrent{ID: id}, nil },
	}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	if tr := getTorrent(t, st, hash); tr.State != store.StateTorBoxActive {
		t.Errorf("state = %q, want still TORBOX_ACTIVE (confirm found it)", tr.State)
	}
}

func TestReconcileAdoptsChangedID(t *testing.T) {
	st := newStore(t)
	hash := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	seedActive(t, st, hash, 500, "/downloads/tv", time.Minute)

	// mylist returns the torrent under a new id but the same hash (promoted from
	// the queue): we should adopt the new id and keep going.
	tb := &fakeTB{list: func() ([]torbox.Torrent, error) {
		return []torbox.Torrent{{ID: 777, Hash: hash, Active: true, DownloadState: "downloading", Progress: 0.2}}, nil
	}}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(context.Background()); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}
	tr := getTorrent(t, st, hash)
	if tr.State != store.StateTorBoxActive {
		t.Fatalf("state = %q, want still TORBOX_ACTIVE", tr.State)
	}
	if !tr.TorBoxID.Valid || tr.TorBoxID.Int64 != 777 {
		t.Errorf("torbox_id = %v, want 777 (adopted)", tr.TorBoxID)
	}
}

// A COMPLETE torrent still present on TorBox is reaped: deleted from the account
// and its refs cleared, freeing the slot. This is the self-heal for completed
// torrents that would otherwise seed forever and starve the queue.
func TestReconcileReapsCompletedFromTorBox(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	hash := "abababababababababababababababababababab"

	tr := seedActive(t, st, hash, 500, "/downloads/tv", 0)
	if err := st.MarkComplete(ctx, tr.ID, "/downloads/tv/show", 10); err != nil {
		t.Fatalf("mark complete: %v", err)
	}

	var deleted int
	var deletedOp torbox.Operation
	tb := &fakeTB{
		list: func() ([]torbox.Torrent, error) {
			return []torbox.Torrent{{ID: 500, Hash: hash}}, nil // still on TorBox
		},
		ctl: func(_ int, op torbox.Operation) error { deleted++; deletedOp = op; return nil },
	}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(ctx); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}

	if deleted != 1 || deletedOp != torbox.OpDelete {
		t.Errorf("ControlTorrent = (calls %d, op %q), want (1, delete)", deleted, deletedOp)
	}
	if n, err := st.CountOnTorBox(ctx); err != nil || n != 0 {
		t.Errorf("CountOnTorBox = %d (err %v), want 0 (slot freed)", n, err)
	}
}

// A COMPLETE torrent already gone from TorBox (e.g. deleted out-of-band) is not
// re-deleted; the reaper just clears our accounting so the slot frees.
func TestReconcileReapClearsRefsWhenAlreadyGone(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	hash := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"

	tr := seedActive(t, st, hash, 600, "/downloads/tv", 0)
	if err := st.MarkComplete(ctx, tr.ID, "/downloads/tv/show", 10); err != nil {
		t.Fatalf("mark complete: %v", err)
	}

	var deleteCalls int
	tb := &fakeTB{
		list: func() ([]torbox.Torrent, error) { return []torbox.Torrent{}, nil }, // not on TorBox
		ctl:  func(int, torbox.Operation) error { deleteCalls++; return nil },
	}
	e := New(st, tb, Config{MaxSlots: 3}, nil)
	if err := e.reconcilePass(ctx); err != nil {
		t.Fatalf("reconcilePass: %v", err)
	}

	if deleteCalls != 0 {
		t.Errorf("ControlTorrent calls = %d, want 0 (already gone, nothing to delete)", deleteCalls)
	}
	if n, err := st.CountOnTorBox(ctx); err != nil || n != 0 {
		t.Errorf("CountOnTorBox = %d (err %v), want 0 (refs cleared)", n, err)
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
