// Package torbox is a thin client for the TorBox API: the few endpoints
// tordownloader needs (createtorrent, mylist, checkcached, requestdl,
// controltorrent, controlqueued), with bearer auth and rate-limit/backoff
// handling.
//
// Rate limits to respect (TorBox): general 300/min, createtorrent 60/hour.
// This client retries 429/5xx with exponential backoff honoring Retry-After.
package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultBaseURL    = "https://api.torbox.app"
	defaultAPIVersion = "v1"
	maxBackoff        = 30 * time.Second
	// defaultRatePerMin caps all TorBox API calls just under the account's general
	// 300/min limit, with headroom for timing slop. Every endpoint shares this
	// budget; staying under it means a burst of requestdl links (a big pack of
	// small files) is paced instead of bouncing off a 429.
	defaultRatePerMin = 280
)

// Client talks to the TorBox API.
type Client struct {
	apiKey         string
	baseURL        string
	apiVersion     string
	httpClient     *http.Client
	maxRetries     int
	initialBackoff time.Duration
	// limiter paces outbound API calls to stay under TorBox's general rate limit.
	// nil means unlimited. Shared across all goroutines using the client (the
	// parallel downloaders, reconciler, and submitter), so the cap is global.
	limiter *rate.Limiter
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets a custom *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithBaseURL overrides the API base URL (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithMaxRetries sets how many times to retry transient (429/5xx) responses.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// WithInitialBackoff sets the first retry delay (subsequent delays grow).
func WithInitialBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.initialBackoff = d
		}
	}
}

// WithRateLimit caps the sustained rate of TorBox API calls to perMinute
// requests, smoothing bursts (e.g. requestdl links for a big pack) so they stay
// under TorBox's general 300/min limit instead of tripping a 429. A small burst
// lets the parallel downloaders grab links without artificial stalls while the
// average holds. perMinute <= 0 disables the limiter (unlimited).
func WithRateLimit(perMinute int) Option {
	return func(c *Client) {
		c.limiter = newLimiter(perMinute)
	}
}

// newLimiter builds a token-bucket limiter for perMinute requests, or nil to
// disable. The burst is ~one second of budget (min 1) so a handful of parallel
// link requests proceed at once without pushing the windowed total over the cap.
func newLimiter(perMinute int) *rate.Limiter {
	if perMinute <= 0 {
		return nil
	}
	burst := perMinute/60 + 1
	return rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), burst)
}

// New returns a TorBox client authenticated with apiKey.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:         apiKey,
		baseURL:        defaultBaseURL,
		apiVersion:     defaultAPIVersion,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
		maxRetries:     4,
		initialBackoff: 500 * time.Millisecond,
		limiter:        newLimiter(defaultRatePerMin),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// CreateTorrent submits a magnet or .torrent file to TorBox.
func (c *Client) CreateTorrent(ctx context.Context, r CreateTorrentRequest) (*CreateTorrentResult, error) {
	if r.Magnet == "" && len(r.File) == 0 {
		return nil, fmt.Errorf("torbox: createtorrent requires a magnet or a file")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if r.Magnet != "" {
		_ = w.WriteField("magnet", r.Magnet)
	}
	if len(r.File) > 0 {
		name := r.FileName
		if name == "" {
			name = "upload.torrent"
		}
		fw, err := w.CreateFormFile("file", name)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(r.File); err != nil {
			return nil, err
		}
	}
	if r.Name != "" {
		_ = w.WriteField("name", r.Name)
	}
	if r.Seed > 0 {
		_ = w.WriteField("seed", strconv.Itoa(r.Seed))
	}
	if r.AllowZip != nil {
		_ = w.WriteField("allow_zip", strconv.FormatBool(*r.AllowZip))
	}
	if r.AsQueued {
		_ = w.WriteField("as_queued", "true")
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	env, err := c.do(ctx, http.MethodPost, "torrents/createtorrent", nil, buf.Bytes(), w.FormDataContentType(), true)
	if err != nil {
		return nil, err
	}
	var out CreateTorrentResult
	if err := unmarshalData(env, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MyList returns all torrents on the account. Pass bypassCache to force a fresh
// read (TorBox caches mylist ~10 min otherwise).
func (c *Client) MyList(ctx context.Context, bypassCache bool) ([]Torrent, error) {
	q := url.Values{}
	q.Set("limit", "1000")
	if bypassCache {
		q.Set("bypass_cache", "true")
	}
	env, err := c.do(ctx, http.MethodGet, "torrents/mylist", q, nil, "", true)
	if err != nil {
		return nil, err
	}
	var out []Torrent
	if err := unmarshalData(env, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTorrent returns a single torrent by its TorBox id.
func (c *Client) GetTorrent(ctx context.Context, id int, bypassCache bool) (*Torrent, error) {
	q := url.Values{}
	q.Set("id", strconv.Itoa(id))
	if bypassCache {
		q.Set("bypass_cache", "true")
	}
	env, err := c.do(ctx, http.MethodGet, "torrents/mylist", q, nil, "", true)
	if err != nil {
		return nil, err
	}
	var out Torrent
	if err := unmarshalData(env, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckCached reports which of the given infohashes are cached on TorBox.
// Returns a map keyed by hash; missing keys mean "not cached".
func (c *Client) CheckCached(ctx context.Context, hashes []string, listFiles bool) (map[string]CachedInfo, error) {
	out := map[string]CachedInfo{}
	if len(hashes) == 0 {
		return out, nil
	}
	q := url.Values{}
	q.Set("hash", strings.Join(hashes, ","))
	q.Set("format", "object")
	if listFiles {
		q.Set("list_files", "true")
	}
	env, err := c.do(ctx, http.MethodGet, "torrents/checkcached", q, nil, "", true)
	if err != nil {
		return nil, err
	}
	// When nothing is cached, TorBox may return an empty array rather than an
	// object — treat that as "none cached".
	trimmed := bytes.TrimSpace(env.Data)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		return out, nil
	}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, fmt.Errorf("torbox: decode checkcached: %w", err)
	}
	return out, nil
}

// RequestDL returns a time-limited CDN URL to download a file (or zip) from a
// finished torrent. Links are valid ~3h. The API key is passed as the `token`
// query parameter (per the TorBox docs) in addition to the bearer header.
func (c *Client) RequestDL(ctx context.Context, p RequestDLParams) (string, error) {
	q := url.Values{}
	q.Set("token", c.apiKey)
	q.Set("torrent_id", strconv.Itoa(p.TorrentID))
	if p.FileID != nil {
		q.Set("file_id", strconv.Itoa(*p.FileID))
	}
	if p.ZipLink {
		q.Set("zip_link", "true")
	}
	if p.UserIP != "" {
		q.Set("user_ip", p.UserIP)
	}
	q.Set("redirect", "false")

	env, err := c.do(ctx, http.MethodGet, "torrents/requestdl", q, nil, "", true)
	if err != nil {
		return "", err
	}
	var link string
	if err := unmarshalData(env, &link); err != nil {
		return "", err
	}
	return link, nil
}

// ControlTorrent performs an operation (reannounce/delete/resume) on a torrent.
func (c *Client) ControlTorrent(ctx context.Context, torrentID int, op Operation) error {
	body, err := json.Marshal(map[string]any{"torrent_id": torrentID, "operation": string(op)})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "torrents/controltorrent", nil, body, "application/json", true)
	return err
}

// DeleteTorrent removes a torrent (and its data) from the TorBox account.
func (c *Client) DeleteTorrent(ctx context.Context, torrentID int) error {
	return c.ControlTorrent(ctx, torrentID, OpDelete)
}

// ControlQueued performs an operation (delete) on a *queued* download — one
// TorBox accepted but hasn't promoted into an active slot yet. Queued downloads
// live in a separate namespace from active torrents: their queued_id is not a
// torrent_id, so controltorrent (and the web UI's delete) won't touch them. They
// must be removed through this dedicated endpoint instead.
func (c *Client) ControlQueued(ctx context.Context, queuedID int, op Operation) error {
	body, err := json.Marshal(map[string]any{
		"queued_id": queuedID,
		"operation": string(op),
		"type":      "torrent",
	})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "queued/controlqueued", nil, body, "application/json", true)
	return err
}

// DeleteQueued removes a queued download from the TorBox account.
func (c *Client) DeleteQueued(ctx context.Context, queuedID int) error {
	return c.ControlQueued(ctx, queuedID, OpDelete)
}

// do performs an HTTP request to the TorBox API with retries on 429/5xx, parses
// the envelope, and converts a non-success response into an *APIError.
func (c *Client) do(ctx context.Context, method, endpoint string, query url.Values, body []byte, contentType string, auth bool) (*Envelope, error) {
	u := fmt.Sprintf("%s/%s/api/%s", c.baseURL, c.apiVersion, endpoint)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	backoff := c.initialBackoff
	for attempt := 0; ; attempt++ {
		// Consume a rate-limit token before every request (retries included), so
		// the global cap holds across all concurrent callers. Blocks until a token
		// is free or ctx is cancelled.
		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, fmt.Errorf("torbox: rate limit wait: %w", err)
			}
		}
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return nil, err
		}
		if auth && c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < c.maxRetries && ctx.Err() == nil {
				if werr := c.wait(ctx, backoff); werr != nil {
					return nil, werr
				}
				backoff = nextBackoff(backoff)
				continue
			}
			return nil, fmt.Errorf("torbox request: %w", err)
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if attempt < c.maxRetries {
				if werr := c.wait(ctx, retryAfter(resp.Header, backoff)); werr != nil {
					return nil, werr
				}
				backoff = nextBackoff(backoff)
				continue
			}
		}

		var env Envelope
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, &env); err != nil {
				// Non-JSON body. On an error status (e.g. a gateway/proxy answering a
				// throttled requestdl with a plain-text "rate limit exceeded" 429,
				// not the JSON envelope) surface a typed APIError so callers can
				// react to the status — otherwise a rate limit looks like a decode
				// bug and can't be retried or backed off. Only a success status with
				// an unparseable body is a genuine decode failure.
				if resp.StatusCode >= 400 {
					return nil, &APIError{StatusCode: resp.StatusCode, Detail: strings.TrimSpace(truncate(respBody, 200))}
				}
				return nil, fmt.Errorf("torbox: decode response (http %d): %w; body=%s", resp.StatusCode, err, truncate(respBody, 200))
			}
		}
		if !env.Success {
			return &env, &APIError{StatusCode: resp.StatusCode, Code: env.errorCode(), Detail: env.Detail}
		}
		return &env, nil
	}
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	// +/- ~10% jitter to avoid thundering herd.
	jitter := time.Duration(rand.Int63n(int64(next/5)+1)) - next/10
	return next + jitter
}

func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return fallback
}

func unmarshalData(env *Envelope, out any) error {
	if out == nil || len(env.Data) == 0 || string(bytes.TrimSpace(env.Data)) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("torbox: decode data: %w", err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
