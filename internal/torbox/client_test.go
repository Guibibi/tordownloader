package torbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("test-key",
		WithBaseURL(srv.URL),
		WithInitialBackoff(time.Millisecond),
	)
}

func TestCreateTorrent(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.URL.Path; got != "/v1/api/torrents/createtorrent" {
			t.Errorf("path = %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if got := r.FormValue("magnet"); got != "magnet:?xt=urn:btih:abc" {
			t.Errorf("magnet = %q", got)
		}
		if got := r.FormValue("name"); got != "My Release" {
			t.Errorf("name = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"error":false,"detail":"ok","data":{"torrent_id":123,"hash":"abc","name":"My Release"}}`))
	})

	res, err := c.CreateTorrent(context.Background(), CreateTorrentRequest{
		Magnet: "magnet:?xt=urn:btih:abc",
		Name:   "My Release",
	})
	if err != nil {
		t.Fatalf("CreateTorrent: %v", err)
	}
	if res.TorrentID == nil || *res.TorrentID != 123 {
		t.Errorf("torrent_id = %v, want 123", res.TorrentID)
	}
	if res.Hash != "abc" {
		t.Errorf("hash = %q", res.Hash)
	}
}

func TestMyList(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/torrents/mylist" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[
			{"id":1,"hash":"aa","name":"T1","size":100,"download_state":"downloading","download_present":false,"progress":0.5,
			 "files":[{"id":0,"name":"T1/a.mkv","short_name":"a.mkv","size":100}]},
			{"id":2,"hash":"bb","name":"T2","download_present":true,"download_finished":true,"progress":1}
		]}`))
	})

	list, err := c.MyList(context.Background(), true)
	if err != nil {
		t.Fatalf("MyList: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].DownloadState != "downloading" || list[0].Progress != 0.5 {
		t.Errorf("torrent[0] = %+v", list[0])
	}
	if len(list[0].Files) != 1 || list[0].Files[0].Name != "T1/a.mkv" {
		t.Errorf("files = %+v", list[0].Files)
	}
	if !list[1].DownloadPresent {
		t.Errorf("torrent[1] should be present")
	}
}

func TestAPIError_TooLarge(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"error":"DOWNLOAD_TOO_LARGE","detail":"Download exceeds plan limit"}`))
	})

	_, err := c.CreateTorrent(context.Background(), CreateTorrentRequest{Magnet: "magnet:?xt=urn:btih:big"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if !apiErr.IsTooLarge() {
		t.Errorf("IsTooLarge() = false, code = %q", apiErr.Code)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
}

func TestRequestDL(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/torrents/requestdl" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("token"); got != "test-key" {
			t.Errorf("token = %q, want test-key", got)
		}
		if got := r.URL.Query().Get("torrent_id"); got != "123" {
			t.Errorf("torrent_id = %q", got)
		}
		if got := r.URL.Query().Get("file_id"); got != "4" {
			t.Errorf("file_id = %q", got)
		}
		_, _ = w.Write([]byte(`{"success":true,"data":"https://cdn.torbox.app/dl/abc.mkv"}`))
	})

	fileID := 4
	link, err := c.RequestDL(context.Background(), RequestDLParams{TorrentID: 123, FileID: &fileID})
	if err != nil {
		t.Fatalf("RequestDL: %v", err)
	}
	if link != "https://cdn.torbox.app/dl/abc.mkv" {
		t.Errorf("link = %q", link)
	}
}

func TestControlTorrent(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/torrents/controltorrent" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			TorrentID int    `json:"torrent_id"`
			Operation string `json:"operation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.TorrentID != 7 || body.Operation != "delete" {
			t.Errorf("body = %+v", body)
		}
		_, _ = w.Write([]byte(`{"success":true,"detail":"deleted"}`))
	})

	if err := c.DeleteTorrent(context.Background(), 7); err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
}

func TestPlainTextErrorBodyIsTypedAPIError(t *testing.T) {
	// TorBox's gateway can answer a throttled requestdl with a 429 and a
	// plain-text body ("rate limit exceeded") rather than the JSON envelope. The
	// client must surface that as a typed *APIError carrying the status — not a
	// JSON-decode error — so callers (the downloader's link retry, the engine's
	// createtorrent cooldown) can recognize and react to the rate limit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limit exceeded"))
	}))
	defer srv.Close()
	c := New("test-key", WithBaseURL(srv.URL), WithInitialBackoff(time.Millisecond), WithMaxRetries(0))

	fileID := 4
	_, err := c.RequestDL(context.Background(), RequestDLParams{TorrentID: 123, FileID: &fileID})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError (got: %v)", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
}

func TestRetryOn429(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"success":false,"error":"RATE_LIMITED","detail":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	})

	if _, err := c.MyList(context.Background(), false); err != nil {
		t.Fatalf("MyList after retry: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server calls = %d, want 2 (1 rate-limited + 1 success)", got)
	}
}
