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
