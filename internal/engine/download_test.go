package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// seedLocalQueued adds a torrent, drives it to LOCAL_QUEUED, and records its
// files — the state the downloader picks up.
func seedLocalQueued(t *testing.T, st *store.Store, hash, savePath string, files []store.FileInput) store.Torrent {
	t.Helper()
	ctx := context.Background()
	tr, _, err := st.AddTorrent(ctx, store.AddTorrentParams{
		Infohash: hash, Name: "show", SavePath: savePath,
		Magnet: "magnet:?xt=urn:btih:" + hash,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.MarkActive(ctx, tr.ID, 4242); err != nil {
		t.Fatalf("mark active: %v", err)
	}
	cp := filepath.Join(savePath, "show")
	if err := st.MarkLocalQueued(ctx, tr.ID, cp, 0, files); err != nil {
		t.Fatalf("mark local queued: %v", err)
	}
	got, _, err := st.GetTorrent(ctx, hash)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

func TestDownloadPassCompletes(t *testing.T) {
	e1 := bytes.Repeat([]byte("1"), 1200)
	e2 := bytes.Repeat([]byte("2"), 800)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/0":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(e1))
		case "/1":
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(e2))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: int64(len(e1))},
		{TorBoxFileID: 1, RelPath: "show/e02.mkv", ShortName: "e02.mkv", Size: int64(len(e2))},
	})

	tb := &fakeTB{dl: func(p torbox.RequestDLParams) (string, error) {
		if p.TorrentID != 4242 || p.FileID == nil {
			t.Errorf("unexpected requestdl params: %+v", p)
		}
		return srv.URL + "/" + strconv.Itoa(*p.FileID), nil
	}}
	e := New(st, tb, Config{ParallelFiles: 2, IncompleteSubdir: ".incomplete"}, nil)

	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait() // downloads run in goroutines; wait before asserting


	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE", tr.State)
	}
	if tr.LocalProgress != 1 {
		t.Errorf("local_progress = %v, want 1", tr.LocalProgress)
	}
	if tr.CompletedOn == 0 {
		t.Error("completed_on not set")
	}
	wantCP := filepath.Join(save, "show")
	if tr.ContentPath != wantCP {
		t.Errorf("content_path = %q, want %q", tr.ContentPath, wantCP)
	}
	assertBytes(t, filepath.Join(save, "show", "e01.mkv"), e1)
	assertBytes(t, filepath.Join(save, "show", "e02.mkv"), e2)

	// Files marked done.
	rows, _ := st.ListFiles(context.Background(), tr.ID)
	for _, f := range rows {
		if !f.Done {
			t.Errorf("file %s not marked done", f.RelPath)
		}
	}
}

// TestDownloadPassDeletesFromTorBoxOnComplete asserts that once the content is on
// local disk, the engine deletes the torrent from TorBox to free the account's
// scarce slot — without waiting for Sonarr's delete (which may be disabled).
func TestDownloadPassDeletesFromTorBoxOnComplete(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "cccccccccccccccccccccccccccccccccccccccc"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", ShortName: "e01.mkv", Size: int64(len(body))},
	})

	var ctlID int
	var ctlOp torbox.Operation
	var ctlCalled int
	tb := &fakeTB{
		dl:  func(torbox.RequestDLParams) (string, error) { return srv.URL, nil },
		ctl: func(id int, op torbox.Operation) error { ctlCalled++; ctlID = id; ctlOp = op; return nil },
	}
	e := New(st, tb, Config{ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait() // downloads run in goroutines; wait before asserting


	tr, _, _ := st.GetTorrent(context.Background(), hash)
	if tr.State != store.StateComplete {
		t.Fatalf("state = %q, want COMPLETE", tr.State)
	}
	// Slot freed on TorBox, but local content stays so Sonarr can still import.
	if ctlCalled != 1 || ctlOp != torbox.OpDelete || ctlID != 4242 {
		t.Errorf("ControlTorrent calls=%d id=%d op=%q, want 1 delete on id 4242", ctlCalled, ctlID, ctlOp)
	}
	assertBytes(t, filepath.Join(save, "show", "e01.mkv"), body)
}

func TestDownloadPassFailsOnLinkError(t *testing.T) {
	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	seedLocalQueued(t, st, hash, save, []store.FileInput{
		{TorBoxFileID: 0, RelPath: "show/e01.mkv", Size: 100},
	})

	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) {
		return "", &torbox.APIError{StatusCode: 500, Detail: "boom"}
	}}
	e := New(st, tb, Config{ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait() // downloads run in goroutines; wait before asserting

	if tr, _, _ := st.GetTorrent(context.Background(), hash); tr.State != store.StateError {
		t.Errorf("state = %q, want ERROR", tr.State)
	}
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %d bytes, want %d", path, len(got), len(want))
	}
}

// TestDownloadPassParallelTorrents asserts that up to parallel_torrents
// downloads run concurrently, no torrent is double-started by repeated passes,
// and torrents beyond the limit wait for a free slot.
func TestDownloadPassParallelTorrents(t *testing.T) {
	release := make(chan struct{})
	body := bytes.Repeat([]byte("p"), 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold every download open until the test releases them
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	st := newStore(t)
	save := filepath.Join(t.TempDir(), "tv")
	hashes := []string{
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333",
		"4444444444444444444444444444444444444444",
	}
	for _, h := range hashes {
		seedLocalQueued(t, st, h, save, []store.FileInput{
			{TorBoxFileID: 0, RelPath: "show-" + h[:4] + "/e01.mkv", ShortName: "e01.mkv", Size: int64(len(body))},
		})
	}

	tb := &fakeTB{dl: func(torbox.RequestDLParams) (string, error) { return srv.URL, nil }}
	e := New(st, tb, Config{ParallelTorrents: 3, ParallelFiles: 1, IncompleteSubdir: ".incomplete"}, nil)

	countActive := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.activeDownloads)
	}

	// Two passes back to back: the limit must hold and nothing double-starts.
	for i := 0; i < 2; i++ {
		if err := e.downloadPass(context.Background()); err != nil {
			t.Fatalf("downloadPass: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for countActive() != 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countActive(); got != 3 {
		t.Fatalf("active downloads = %d, want 3 (the limit)", got)
	}

	close(release)
	e.dlWG.Wait()

	// Three done, the fourth still waiting its turn; another pass finishes it.
	if n := countState(t, st, store.StateComplete); n != 3 {
		t.Fatalf("complete after first wave = %d, want 3", n)
	}
	if err := e.downloadPass(context.Background()); err != nil {
		t.Fatalf("downloadPass: %v", err)
	}
	e.dlWG.Wait()
	if n := countState(t, st, store.StateComplete); n != 4 {
		t.Fatalf("complete after second wave = %d, want 4", n)
	}
}
