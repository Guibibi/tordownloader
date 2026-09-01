package downloader

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// contentServer serves byte payloads keyed by URL path, with Range support
// (via http.ServeContent), so resume can be exercised.
func contentServer(payloads map[string][]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := payloads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(b))
	}))
}

func staticLink(url string) LinkFunc {
	return func(context.Context, int) (string, error) { return url, nil }
}

func TestDownloadWritesFiles(t *testing.T) {
	a := bytes.Repeat([]byte("a"), 1000)
	b := bytes.Repeat([]byte("b"), 500)
	srv := contentServer(map[string][]byte{"/0": a, "/1": b})
	defer srv.Close()

	dir := t.TempDir()
	files := []File{
		{RowID: 1, TorBoxID: 0, Dest: filepath.Join(dir, "show", "e01.mkv"), Size: int64(len(a))},
		{RowID: 2, TorBoxID: 1, Dest: filepath.Join(dir, "show", "e02.mkv"), Size: int64(len(b))},
	}
	link := func(_ context.Context, id int) (string, error) {
		return srv.URL + "/" + map[int]string{0: "0", 1: "1"}[id], nil
	}

	var done int64 // FileDone may run concurrently; count atomically
	err := Download(context.Background(), files, link, Options{
		Parallel: 2,
		FileDone: func(int64, int64) { atomic.AddInt64(&done, 1) },
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, files[0].Dest, a)
	assertFile(t, files[1].Dest, b)
	if done != 2 {
		t.Errorf("FileDone called %d times, want 2", done)
	}
}

func TestDownloadResumesPartialFile(t *testing.T) {
	full := bytes.Repeat([]byte("x"), 2000)
	srv := contentServer(map[string][]byte{"/f": full})
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	// Pre-seed the first 800 bytes as a partial download.
	if err := os.WriteFile(dest, full[:800], 0o644); err != nil {
		t.Fatal(err)
	}

	var lastProgress int64
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(full))}},
		staticLink(srv.URL+"/f"),
		Options{Progress: func(n int64) { lastProgress = n }, ProgressInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, full)
	if lastProgress != int64(len(full)) {
		t.Errorf("final progress = %d, want %d", lastProgress, len(full))
	}
}

func TestDownloadAlreadyComplete(t *testing.T) {
	data := []byte("complete")
	// Server would 404 — proving no request is made when the file is already done.
	srv := contentServer(map[string][]byte{})
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "done.bin")
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(data))}},
		staticLink(srv.URL+"/missing"),
		Options{})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
}

func TestDownloadSizeMismatch(t *testing.T) {
	data := bytes.Repeat([]byte("z"), 100)
	srv := contentServer(map[string][]byte{"/f": data})
	defer srv.Close()

	dir := t.TempDir()
	err := Download(context.Background(),
		[]File{{Dest: filepath.Join(dir, "f.bin"), Size: int64(len(data)) + 50}}, // wrong expected size
		staticLink(srv.URL+"/f"),
		Options{})
	if err == nil {
		t.Fatal("expected a size-mismatch error")
	}
}

func TestDownloadReRequestsExpiredLink(t *testing.T) {
	data := bytes.Repeat([]byte("q"), 300)
	mux := http.NewServeMux()
	mux.HandleFunc("/expired", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	mux.HandleFunc("/good", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	calls := 0
	link := func(context.Context, int) (string, error) {
		calls++
		if calls == 1 {
			return srv.URL + "/expired", nil
		}
		return srv.URL + "/good", nil
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := Download(context.Background(), []File{{Dest: dest, Size: int64(len(data))}}, link, Options{}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if calls < 2 {
		t.Errorf("link requested %d times, want >= 2 (re-request after expiry)", calls)
	}
}

func TestDownloadRetriesTransientStatus(t *testing.T) {
	// Shrink the backoff so the retry path runs instantly.
	oldBase, oldMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, time.Millisecond
	defer func() { retryBaseDelay, retryMaxDelay = oldBase, oldMax }()

	data := bytes.Repeat([]byte("q"), 300)
	var hits int32
	mux := http.NewServeMux()
	// First two GETs blip with a transient 400 (the real-world failure: a CDN
	// link returning 400 mid-fetch); the third succeeds.
	mux.HandleFunc("/f", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var calls int32
	link := func(context.Context, int) (string, error) {
		atomic.AddInt32(&calls, 1)
		return srv.URL + "/f", nil
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := Download(context.Background(), []File{{Dest: dest, Size: int64(len(data))}}, link, Options{}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if calls < 3 {
		t.Errorf("link requested %d times, want >= 3 (re-request after each transient 400)", calls)
	}
}

func TestDownloadRetriesLinkRequestError(t *testing.T) {
	// Shrink the backoff so the retry path runs instantly.
	oldBase, oldMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, time.Millisecond
	defer func() { retryBaseDelay, retryMaxDelay = oldBase, oldMax }()

	data := bytes.Repeat([]byte("q"), 300)
	srv := contentServer(map[string][]byte{"/f": data})
	defer srv.Close()

	// The link request is rate-limited the first two times — the real failure that
	// killed a 362-file pack: requestdl 429s when a burst of small files hammers
	// it. The torrent must ride it out (re-request after backoff), not fail.
	var calls int32
	link := func(context.Context, int) (string, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return "", errors.New("torbox: rate limit exceeded [http 429]")
		}
		return srv.URL + "/f", nil
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := Download(context.Background(), []File{{Dest: dest, Size: int64(len(data))}}, link, Options{}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if calls < 3 {
		t.Errorf("link requested %d times, want >= 3 (retry after rate-limited link request)", calls)
	}
}

func TestDownloadFailsAfterPersistentLinkError(t *testing.T) {
	// A link request that never recovers must still fail eventually (bounded
	// retries), so a genuinely dead torrent surfaces to Sonarr rather than
	// re-requesting forever.
	oldBase, oldMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, time.Millisecond
	defer func() { retryBaseDelay, retryMaxDelay = oldBase, oldMax }()

	var calls int32
	link := func(context.Context, int) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", errors.New("torbox: rate limit exceeded [http 429]")
	}
	err := Download(context.Background(),
		[]File{{Dest: filepath.Join(t.TempDir(), "f.bin"), Size: 100}}, link, Options{})
	if err == nil {
		t.Fatal("expected an error after exhausting link-request retries")
	}
	if calls < 2 {
		t.Errorf("link requested %d times, want multiple retries before giving up", calls)
	}
}

func TestDownloadFailsAfterPersistentTransient(t *testing.T) {
	// Always-400 link: exhausts retries and returns an error (a genuinely dead
	// file should still fail the torrent so Sonarr blacklists the release).
	oldBase, oldMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Millisecond, time.Millisecond
	defer func() { retryBaseDelay, retryMaxDelay = oldBase, oldMax }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	dir := t.TempDir()
	err := Download(context.Background(),
		[]File{{Dest: filepath.Join(dir, "f.bin"), Size: 100}},
		staticLink(srv.URL+"/f"), Options{})
	if err == nil {
		t.Fatal("expected an error after exhausting transient retries")
	}
}

func TestFinalizeSingleFolder(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, ".incomplete", "hash")
	save := filepath.Join(dir, "tv")
	mustWrite(t, filepath.Join(staging, "show", "e01.mkv"), []byte("one"))
	mustWrite(t, filepath.Join(staging, "show", "e02.mkv"), []byte("two"))

	cp, err := Finalize(staging, save)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if want := filepath.Join(save, "show"); cp != want {
		t.Errorf("content_path = %q, want %q", cp, want)
	}
	assertFile(t, filepath.Join(save, "show", "e01.mkv"), []byte("one"))
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging dir should be removed, stat err = %v", err)
	}
}

func TestFinalizeSingleFile(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, ".incomplete", "hash")
	save := filepath.Join(dir, "movies")
	mustWrite(t, filepath.Join(staging, "movie.mkv"), []byte("film"))

	cp, err := Finalize(staging, save)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if want := filepath.Join(save, "movie.mkv"); cp != want {
		t.Errorf("content_path = %q, want %q", cp, want)
	}
	assertFile(t, filepath.Join(save, "movie.mkv"), []byte("film"))
}

func TestFinalizeOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, ".incomplete", "hash")
	save := filepath.Join(dir, "movies")
	mustWrite(t, filepath.Join(staging, "movie.mkv"), []byte("new"))
	mustWrite(t, filepath.Join(save, "movie.mkv"), []byte("stale"))

	if _, err := Finalize(staging, save); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	assertFile(t, filepath.Join(save, "movie.mkv"), []byte("new"))
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %d bytes, want %d bytes", path, len(got), len(want))
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadTruncatedTransferRetries(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/f", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// Clean 200 but only half the bytes: a truncated transfer the size
			// verification must catch and retry, not fail the torrent over.
			_, _ = w.Write(data[:50])
			return
		}
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	var lastProgress int64
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(data))}},
		staticLink(srv.URL+"/f"),
		Options{Progress: func(n int64) { lastProgress = n }})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if calls < 2 {
		t.Errorf("server calls = %d, want a retry after the truncated transfer", calls)
	}
	// The discarded 50 bytes must have been subtracted, not left inflating the total.
	if lastProgress != int64(len(data)) {
		t.Errorf("final progress = %d, want %d", lastProgress, len(data))
	}
}

func TestDownloadOversizePartialProgressNotInflated(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 100)
	srv := contentServer(map[string][]byte{"/f": data})
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	// Oversize/corrupt leftover from a previous run: must be discarded, never counted.
	if err := os.WriteFile(dest, bytes.Repeat([]byte("junk"), 100), 0o644); err != nil {
		t.Fatal(err)
	}

	var lastProgress int64
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(data))}},
		staticLink(srv.URL+"/f"),
		Options{Progress: func(n int64) { lastProgress = n }})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if lastProgress != int64(len(data)) {
		t.Errorf("final progress = %d, want %d", lastProgress, len(data))
	}
}

func TestDownloadDroppedConnectionResumes(t *testing.T) {
	data := bytes.Repeat([]byte("r"), 4000)
	restoreDelays := func(base, max time.Duration) { retryBaseDelay, retryMaxDelay = base, max }
	defer restoreDelays(retryBaseDelay, retryMaxDelay)
	retryBaseDelay, retryMaxDelay = time.Millisecond, time.Millisecond

	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/f", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// Advertise the full length but drop the connection halfway: the client
			// sees an unexpected EOF mid-body.
			w.Header().Set("Content-Length", "4000")
			_, _ = w.Write(data[:2000])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(data))}},
		staticLink(srv.URL+"/f"),
		Options{})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if calls < 2 {
		t.Errorf("server calls = %d, want a resume after the dropped connection", calls)
	}
}

func TestDownloadIdleTransferAbortsAndResumes(t *testing.T) {
	data := bytes.Repeat([]byte("i"), 4000)
	restoreDelays := func(base, max time.Duration) { retryBaseDelay, retryMaxDelay = base, max }
	defer restoreDelays(retryBaseDelay, retryMaxDelay)
	retryBaseDelay, retryMaxDelay = time.Millisecond, time.Millisecond

	var calls int32
	released := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/f", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Send half the body, then go silent with the connection still open —
			// the failure mode nothing used to catch, because the client has no
			// overall deadline and the transfer never errors on its own.
			w.Header().Set("Content-Length", "4000")
			_, _ = w.Write(data[:2000])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-released
			return
		}
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(data))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(released)

	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(data))}},
		staticLink(srv.URL+"/f"),
		Options{IdleTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Errorf("server calls = %d, want a retry after the idle transfer was aborted", n)
	}
}

func TestDownloadIdleTimeoutDoesNotCutSlowButMovingTransfer(t *testing.T) {
	// Bytes trickle in well inside the idle timeout. The watchdog measures
	// silence, not throughput, so a slow-but-alive transfer must complete
	// untouched — the speed policy lives in the engine, not here.
	data := bytes.Repeat([]byte("s"), 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "400")
		for i := 0; i < 4; i++ {
			_, _ = w.Write(data[i*100 : (i+1)*100])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "s.bin")
	err := Download(context.Background(),
		[]File{{Dest: dest, Size: int64(len(data))}},
		staticLink(srv.URL+"/s"),
		Options{IdleTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertFile(t, dest, data)
}

func TestDownloadIdleTimeoutHonoursParentCancel(t *testing.T) {
	// A cancelled parent (engine shutdown, or the torrent being deleted) must
	// surface as the context error, not be mistaken for an idle abort and retried.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4000")
		_, _ = w.Write(bytes.Repeat([]byte("c"), 100))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	err := Download(ctx,
		[]File{{Dest: filepath.Join(t.TempDir(), "c.bin"), Size: 4000}},
		staticLink(srv.URL+"/c"),
		Options{IdleTimeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Download err = %v, want context.Canceled", err)
	}
}
