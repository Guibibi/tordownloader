// Package downloader fetches a torrent's files from TorBox CDN links to a local
// staging tree, with bounded concurrency, HTTP Range resume of partial files,
// re-request of expired links, and size verification. Finalize then atomically
// moves the completed content into the save path so consumers (Sonarr/Radarr)
// never see partial files.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxLinkRetries bounds how many times one file re-requests a fresh CDN link
	// (links expire ~3h, and the CDN occasionally returns transient 4xx/5xx)
	// before giving up. With the backoff schedule below a genuinely-dead file
	// still fails within ~1-2 minutes.
	maxLinkRetries          = 5
	defaultProgressInterval = time.Second
	// defaultIdleTimeout is how long a transfer may deliver nothing before it is
	// abandoned and retried. It has to be generous — a CDN can legitimately pause
	// while it seeks or re-buffers — but finite, because the alternative is a
	// transfer that hangs forever: the HTTP client has no overall deadline (a real
	// download runs for hours), so silence is the only thing safe to measure.
	defaultIdleTimeout = 2 * time.Minute
)

// retryBaseDelay/retryMaxDelay shape the exponential backoff between attempts on
// a transient CDN error, so a 429 (rate limit) isn't burned through instantly.
// Expired-link and range-reset retries don't back off — they're expected, not
// transient. They are vars (not consts) so tests can shrink them.
var (
	retryBaseDelay = 2 * time.Second
	retryMaxDelay  = 30 * time.Second
)

// errExpiredLink signals that a CDN link was rejected (expired/unauthorized) and
// the file should be retried with a freshly requested link. errRangeReset
// signals a partial file must be re-fetched from the start. errTransient signals
// a transient CDN failure (e.g. 400/429/5xx) worth retrying with a fresh link
// after a backoff rather than failing the whole torrent over one blip.
var (
	errExpiredLink = errors.New("download link expired")
	errRangeReset  = errors.New("range reset required")
	errTransient   = errors.New("transient download error")
)

// ErrIdle reports that a transfer was aborted because it stopped delivering
// bytes for the idle timeout. It is wrapped in errTransient, so the retry loop
// treats it like any other transient CDN failure: back off, request a fresh
// link, and resume from the bytes already on disk. Exported so callers can
// recognise the cause in a failure message.
var ErrIdle = errors.New("no data received")

// LinkFunc returns a fresh, time-limited CDN URL for the given TorBox file id.
// The downloader calls it again whenever a link is rejected as expired.
type LinkFunc func(ctx context.Context, torboxFileID int) (string, error)

// File is one file to fetch into the staging tree.
type File struct {
	RowID    int64  // store file-row id, passed back via Options.FileDone
	TorBoxID int    // TorBox file id, passed to LinkFunc
	Dest     string // absolute path in the staging tree (folders preserved)
	Size     int64  // expected bytes; 0 = unknown (skips size verification)
}

// Options configures a Download run. Progress is invoked only from a single
// goroutine (safe to use without locking); FileDone may be invoked concurrently
// from multiple file workers, so its body must be safe for concurrent use.
type Options struct {
	Parallel         int                           // max concurrent file downloads (default 1)
	HTTPClient       *http.Client                  // default http.DefaultClient
	Progress         func(downloaded int64)        // aggregate bytes so far, called periodically
	FileDone         func(rowID int64, size int64) // a file finished & verified (may be concurrent)
	ProgressInterval time.Duration                 // Progress cadence (default 1s)
	// IdleTimeout aborts a transfer that has delivered no bytes for this long,
	// covering the whole request: connect, headers, and body. The attempt fails as
	// transient, so it is retried with a fresh link and resumed from disk — a trip
	// costs one backoff, not the torrent. Default 2m; negative disables the
	// watchdog entirely (a hung transfer then blocks its slot until the torrent is
	// deleted or the process restarts).
	IdleTimeout time.Duration
}

// Download fetches every file with bounded concurrency. It returns the first
// error encountered (after which the torrent should be failed); a nil error
// means all files are present and verified in the staging tree.
func Download(ctx context.Context, files []File, link LinkFunc, opts Options) error {
	if opts.Parallel < 1 {
		opts.Parallel = 1
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	interval := opts.ProgressInterval
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	// 0 means "unset" → the default; a negative value disables the watchdog.
	idle := opts.IdleTimeout
	if idle == 0 {
		idle = defaultIdleTimeout
	}

	// Seed the counter with bytes already on disk so resumed downloads report
	// accurate progress; fetch only adds the bytes it newly writes. An
	// oversize/corrupt partial is deleted here rather than counted, so that from
	// this point every on-disk byte is reflected in the counter — which lets
	// discard subtract exactly what it removes.
	var downloaded int64
	for _, f := range files {
		if fi, err := os.Stat(f.Dest); err == nil {
			s := fi.Size()
			if f.Size > 0 && s > f.Size {
				_ = os.Remove(f.Dest)
				continue
			}
			downloaded += s
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Periodic aggregate progress reporter.
	var reporterWG sync.WaitGroup
	if opts.Progress != nil {
		opts.Progress(atomic.LoadInt64(&downloaded)) // immediate first sample
		reporterWG.Add(1)
		go func() {
			defer reporterWG.Done()
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					opts.Progress(atomic.LoadInt64(&downloaded))
				}
			}
		}()
	}

	sem := make(chan struct{}, opts.Parallel)
	errs := make(chan error, len(files))
	var wg sync.WaitGroup
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
			defer func() { <-sem }()

			if err := fetch(ctx, client, f, link, &downloaded, idle); err != nil {
				errs <- fmt.Errorf("%s: %w", filepath.Base(f.Dest), err)
				return
			}
			if opts.FileDone != nil {
				opts.FileDone(f.RowID, f.Size)
			}
			errs <- nil
		}()
	}
	wg.Wait()
	close(errs)
	cancel()
	reporterWG.Wait()
	if opts.Progress != nil {
		opts.Progress(atomic.LoadInt64(&downloaded)) // final, authoritative sample
	}

	var firstErr error
	for e := range errs {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return firstErr
}

// fetch downloads one file, re-requesting a fresh link on expiry and resuming
// from any bytes already on disk.
func fetch(ctx context.Context, client *http.Client, f File, link LinkFunc, downloaded *int64, idle time.Duration) error {
	if f.Size > 0 {
		if fi, err := os.Stat(f.Dest); err == nil && fi.Size() == f.Size {
			return nil // already complete from a previous run
		}
	}
	if err := os.MkdirAll(filepath.Dir(f.Dest), 0o755); err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt <= maxLinkRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		url, err := link(ctx, f.TorBoxID)
		if err != nil {
			// A link request can fail transiently — most often TorBox's general
			// rate limit (429) when many files request links at once (a big pack of
			// small files blows through requestdl). Failing here would fail the
			// whole torrent over one throttled call, so back off and re-request,
			// the same as a transient CDN error; we give up only after the retry
			// budget. A cancelled ctx (shutdown/delete) is never retried.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("request link: %w", err)
			if attempt < maxLinkRetries {
				if werr := backoff(ctx, attempt); werr != nil {
					return werr
				}
			}
			continue
		}
		err = downloadOnce(ctx, client, url, f, downloaded, idle)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errTransient):
			lastErr = err
			// Back off before re-requesting, so a rate-limit (429) or flapping
			// CDN gets a chance to recover instead of burning every retry at once.
			if attempt < maxLinkRetries {
				if werr := backoff(ctx, attempt); werr != nil {
					return werr
				}
			}
			continue
		case errors.Is(err, errExpiredLink), errors.Is(err, errRangeReset):
			lastErr = err
			continue // re-request a link and (for range reset) restart from 0
		default:
			return err
		}
	}
	return lastErr
}

// backoff sleeps for an exponentially growing delay (capped) before the next
// retry attempt, returning early if the context is cancelled.
func backoff(ctx context.Context, attempt int) error {
	d := retryBaseDelay << attempt
	if d > retryMaxDelay || d <= 0 {
		d = retryMaxDelay
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// downloadOnce performs a single GET, resuming via Range from the current
// on-disk size, and verifies the final size.
func downloadOnce(ctx context.Context, client *http.Client, url string, f File, downloaded *int64, idle time.Duration) error {
	var start int64
	if fi, err := os.Stat(f.Dest); err == nil {
		start = fi.Size()
		if f.Size > 0 && start > f.Size {
			discard(f.Dest, start, downloaded)
			start = 0
		}
	}

	// The watchdog owns this request's context, so a silence anywhere in it —
	// connect, headers, or mid-body — aborts the attempt instead of hanging. It is
	// armed before Do and rearmed on every body read that returns.
	reqCtx, watchdog := newIdleWatchdog(ctx, idle)
	defer watchdog.stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := client.Do(req)
	if err != nil {
		return watchdog.classify(ctx, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// Server ignored the Range (or none asked): full body, rewrite. The
		// already-counted partial bytes are about to be truncated away.
		if start > 0 {
			atomic.AddInt64(downloaded, -start)
		}
		start = 0
	case resp.StatusCode == http.StatusPartialContent:
		// resuming as requested
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if f.Size > 0 && start >= f.Size {
			return nil // server says nothing more to send and we have it all
		}
		discard(f.Dest, start, downloaded)
		return errRangeReset
	case isExpiredStatus(resp.StatusCode):
		return errExpiredLink
	case isTransientStatus(resp.StatusCode):
		return fmt.Errorf("%w: http %d", errTransient, resp.StatusCode)
	default:
		return fmt.Errorf("unexpected http status %d", resp.StatusCode)
	}

	flag := os.O_CREATE | os.O_WRONLY
	if start > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	out, err := os.OpenFile(f.Dest, flag, 0o644)
	if err != nil {
		return err
	}
	cw := &countWriter{w: out, total: downloaded}
	_, copyErr := io.Copy(cw, watchdog.wrap(resp.Body))
	closeErr := out.Close()
	if copyErr != nil {
		// A dropped connection mid-body, or a transfer the watchdog gave up on.
		// Partial bytes are kept and counted, so treat it as transient: the next
		// attempt backs off and resumes via Range instead of failing the whole
		// torrent on one broken transfer. (A cancelled parent ctx surfaces at the
		// top of the retry loop instead.)
		return watchdog.classify(ctx, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}

	if f.Size > 0 {
		fi, err := os.Stat(f.Dest)
		if err != nil {
			return err
		}
		if fi.Size() != f.Size {
			// Truncated/corrupt transfer (e.g. a CDN connection that ended early with
			// a clean EOF). Retryable within the attempt budget: drop the bad bytes
			// and re-fetch from scratch rather than failing the whole torrent over
			// one bad transfer.
			discard(f.Dest, fi.Size(), downloaded)
			return fmt.Errorf("size mismatch: got %d, want %d: %w", fi.Size(), f.Size, errRangeReset)
		}
	}
	return nil
}

// discard removes a partial file and subtracts its bytes from the aggregate
// progress counter. Every on-disk byte was previously counted (seeded at
// Download start or written through countWriter), so dropping a file without
// the subtraction would leave reported progress overshooting reality.
func discard(dest string, n int64, downloaded *int64) {
	_ = os.Remove(dest)
	if n > 0 {
		atomic.AddInt64(downloaded, -n)
	}
}

// Finalize atomically moves the staged content into savePath and returns the
// qBittorrent content_path. Each top-level entry of the staging dir is renamed
// into place; the common case (a single file or a single torrent folder) is one
// atomic rename, so consumers never observe a partial import.
func Finalize(stagingDir, savePath string) (string, error) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no downloaded content in %s", stagingDir)
	}
	if err := os.MkdirAll(savePath, 0o755); err != nil {
		return "", err
	}
	for _, e := range entries {
		src := filepath.Join(stagingDir, e.Name())
		dst := filepath.Join(savePath, e.Name())
		if err := os.RemoveAll(dst); err != nil {
			return "", fmt.Errorf("clear %s: %w", dst, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return "", fmt.Errorf("move %s into place: %w", e.Name(), err)
		}
	}
	_ = os.Remove(stagingDir) // best-effort: now empty

	if len(entries) == 1 {
		return filepath.Join(savePath, entries[0].Name()), nil
	}
	return savePath, nil
}

func isExpiredStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	default:
		return false
	}
}

// isTransientStatus reports whether an HTTP status is a transient CDN failure
// worth retrying with a fresh link (after backoff) rather than failing the whole
// torrent. Covers bad-request blips, timeouts, too-early, rate limits, and any
// server-side 5xx. Real qBittorrent CDN links occasionally return 400 mid-fetch.
func isTransientStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusRequestTimeout,
		http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return code >= 500 && code <= 599
}

// countWriter tallies bytes written into an atomic total for progress reporting.
type countWriter struct {
	w     io.Writer
	total *int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	atomic.AddInt64(c.total, int64(n))
	return n, err
}

// idleWatchdog aborts a request that stops delivering bytes. It owns a
// cancellable child of the caller's context: a timer fires the cancel after
// idle elapses, and every read that returns rearms the timer. That single
// mechanism covers the whole request — a dial that never connects, headers that
// never arrive, and a body that goes quiet mid-transfer — which is what makes it
// safe to run the download client with no overall timeout, since a legitimate
// transfer can run for hours but never goes silent for minutes.
//
// A zero or negative idle disables it: the watchdog then just passes the
// caller's context and reader through untouched.
type idleWatchdog struct {
	idle    time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	expired atomic.Bool // set when the timer fired, to tell our abort from any other
}

// newIdleWatchdog arms a watchdog over ctx and returns the context requests
// should use. The caller must call stop when the request is done.
func newIdleWatchdog(ctx context.Context, idle time.Duration) (context.Context, *idleWatchdog) {
	w := &idleWatchdog{idle: idle}
	if idle <= 0 {
		return ctx, w
	}
	reqCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.timer = time.AfterFunc(idle, func() {
		w.expired.Store(true)
		cancel()
	})
	return reqCtx, w
}

// stop releases the watchdog's timer and context. Safe to call more than once.
func (w *idleWatchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
	if w.cancel != nil {
		w.cancel()
	}
}

// wrap returns r with the watchdog rearmed on every read. A disabled watchdog
// returns r unchanged.
func (w *idleWatchdog) wrap(r io.Reader) io.Reader {
	if w.timer == nil {
		return r
	}
	return &idleReader{r: r, w: w}
}

// classify turns a request error into the error the retry loop should see. A
// cancelled *parent* context (shutdown, or the torrent being deleted) is
// returned as-is so the loop stops immediately; a watchdog abort becomes a
// transient failure so the file is retried with a fresh link and resumed from
// disk. Anything else passes through as transient, as a dropped connection
// always did.
func (w *idleWatchdog) classify(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if w.expired.Load() {
		return fmt.Errorf("%w: %w for %s", errTransient, ErrIdle, w.idle)
	}
	return fmt.Errorf("%w: %v", errTransient, err)
}

// idleReader rearms the watchdog around each read, so the deadline measures
// silence rather than total transfer time.
type idleReader struct {
	r io.Reader
	w *idleWatchdog
}

func (ir *idleReader) Read(p []byte) (int, error) {
	// Rearm before blocking: the timer must cover the read we are about to make,
	// not the one that just finished. If it has already fired, the request context
	// is cancelled and this read returns the error that ends the transfer.
	ir.w.timer.Reset(ir.w.idle)
	return ir.r.Read(p)
}
