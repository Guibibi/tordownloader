// Package arr notifies Sonarr/Radarr when a download has failed for good.
//
// Sonarr deliberately never runs failed-download handling for qBittorrent
// clients: every qBittorrent state, including "error", maps to at most a
// Warning ("warning so failed download handling isn't triggered" in Sonarr's
// source). So reporting state=error only paints the queue item orange — the
// release is never blocklisted and no alternative is grabbed, and the item
// sits there until a human removes it. The only way to complete the failure
// loop is what a human would do in the UI: remove the queue item with
// "blocklist" ticked — so this package does exactly that through the *arr API
// (DELETE /api/v3/queue/{id}?removeFromClient=true&blocklist=true&
// skipRedownload=false), which blocklists the release and triggers an
// automatic search for a replacement.
package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Instance is one configured Sonarr/Radarr endpoint.
type Instance struct {
	Name   string // label for logs (e.g. "sonarr")
	URL    string // base URL, e.g. http://sonarr:8989 (base paths supported)
	APIKey string
}

// Notifier fans a failure notification out to every configured instance.
type Notifier struct {
	instances []Instance
	client    *http.Client
	log       *slog.Logger
}

// New builds a Notifier over the given instances. Trailing slashes on URLs are
// normalised away so path joining is uniform.
func New(instances []Instance, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	norm := make([]Instance, 0, len(instances))
	for _, in := range instances {
		in.URL = strings.TrimRight(in.URL, "/")
		norm = append(norm, in)
	}
	return &Notifier{
		instances: norm,
		client:    &http.Client{Timeout: 15 * time.Second},
		log:       log,
	}
}

// VerifyConnections probes each instance's /api/v3/system/status once and logs
// whether the endpoint and API key work — startup feedback so a bad URL or key
// is visible immediately instead of on the first failed download. Best-effort:
// failures are logged, not returned, and don't disable the notifier (the *arr
// may simply not be up yet; notifications retry on their own schedule).
func (n *Notifier) VerifyConnections(ctx context.Context) {
	for _, in := range n.instances {
		st, err := n.systemStatus(ctx, in)
		if err != nil {
			n.log.Warn("arr connection check failed; failure push-back will keep retrying when needed",
				"instance", in.Name, "url", in.URL, "err", err)
			continue
		}
		n.log.Info("arr API connection established",
			"instance", in.Name, "app", st.AppName, "version", st.Version, "url", in.URL)
	}
}

// statusResponse is the slice of GET /api/v3/system/status we log.
type statusResponse struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// systemStatus fetches an instance's identity, doubling as an API-key check
// (the *arrs answer it with 401 on a bad key).
func (n *Notifier) systemStatus(ctx context.Context, in Instance) (*statusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL+"/api/v3/system/status", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", in.APIKey)
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("status 401: rejected API key (check api_key)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var st statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode system/status: %w", err)
	}
	return &st, nil
}

// queuePage is the shape of GET /api/v3/queue (both Sonarr v4 and Radarr v5).
type queuePage struct {
	TotalRecords int           `json:"totalRecords"`
	Records      []queueRecord `json:"records"`
}

// queueRecord is one queue item; DownloadID is the uppercase infohash the *arr
// tracks the grab under. A multi-episode grab yields several records sharing
// one DownloadID.
type queueRecord struct {
	ID         int64  `json:"id"`
	DownloadID string `json:"downloadId"`
	Title      string `json:"title"`
}

// NotifyFailed tells whichever instance grabbed the torrent that it failed:
// its queue records matching the infohash are removed with blocklist=true and
// skipRedownload=false, which blocklists the release and triggers a search for
// a replacement. removeFromClient=true makes the *arr also delete the torrent
// from us, cleaning up the ERROR row and any partial local files.
//
// The returned bool is true when a matching queue item was found and removed.
// (false, nil) means every instance answered and none tracks the hash — the
// caller may retry (a very fast failure can beat the *arr's queue refresh) and
// eventually give up. A non-nil error means at least one instance could not be
// checked, so absence is not yet proven.
func (n *Notifier) NotifyFailed(ctx context.Context, infohash, name string) (bool, error) {
	var firstErr error
	for _, in := range n.instances {
		handled, err := n.notifyOne(ctx, in, infohash, name)
		if err != nil {
			n.log.Warn("arr notify failed (will retry)", "instance", in.Name, "infohash", infohash, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", in.Name, err)
			}
			continue
		}
		if handled {
			return true, nil
		}
	}
	return false, firstErr
}

// notifyOne checks one instance's queue for the infohash and removes+blocklists
// every matching record. Returns whether any record matched.
func (n *Notifier) notifyOne(ctx context.Context, in Instance, infohash, name string) (bool, error) {
	records, err := n.fetchQueue(ctx, in)
	if err != nil {
		return false, err
	}
	handled := false
	for _, r := range records {
		if !strings.EqualFold(r.DownloadID, infohash) {
			continue
		}
		if err := n.removeQueueItem(ctx, in, r.ID); err != nil {
			return handled, fmt.Errorf("remove queue item %d: %w", r.ID, err)
		}
		handled = true
		n.log.Info("failed download blocklisted in *arr; replacement search triggered",
			"instance", in.Name, "infohash", infohash, "name", name, "queue_id", r.ID)
	}
	return handled, nil
}

// fetchQueue reads the instance's queue. One large page is plenty: a home
// *arr queue is far below 1000 items. The includeUnknown* params (one per
// app; each ignores the other's) make sure items the *arr can't map to a
// series/movie still show up.
func (n *Notifier) fetchQueue(ctx context.Context, in Instance) ([]queueRecord, error) {
	q := url.Values{
		"page":                      {"1"},
		"pageSize":                  {"1000"},
		"includeUnknownSeriesItems": {"true"},
		"includeUnknownMovieItems":  {"true"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL+"/api/v3/queue?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", in.APIKey)
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET queue: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page queuePage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode queue: %w", err)
	}
	return page.Records, nil
}

// removeQueueItem removes one queue record with blocklisting on and redownload
// allowed — the *arr blocklists the release and immediately searches for an
// alternative. A 404 means another delete (ours or the *arr's own) already
// removed it, which is success.
func (n *Notifier) removeQueueItem(ctx context.Context, in Instance, id int64) error {
	q := url.Values{
		"removeFromClient": {"true"},
		"blocklist":        {"true"},
		"skipRedownload":   {"false"},
	}
	u := fmt.Sprintf("%s/api/v3/queue/%d?%s", in.URL, id, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", in.APIKey)
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
