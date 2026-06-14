package qbit

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/guibibi/tordownloader/internal/store"
)

const testHash = "c12fe1c06bba254a9dc9f519b335aa7c1367a88a"

func postForm(t *testing.T, url string, vals url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := http.PostForm(url, vals)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestAddMagnetQueuesTorrent(t *testing.T) {
	srv, st := newTestServer(t)
	magnet := "magnet:?xt=urn:btih:" + testHash + "&dn=My.Show.S01E01"

	_, body := postForm(t, srv.URL+"/api/v2/torrents/add", url.Values{
		"urls":     {magnet},
		"category": {"tv-sonarr"},
	})
	if strings.TrimSpace(body) != "Ok." {
		t.Fatalf("add body = %q, want Ok.", body)
	}

	tor, ok, err := st.GetTorrent(context.Background(), testHash)
	if err != nil || !ok {
		t.Fatalf("torrent not persisted: ok=%v err=%v", ok, err)
	}
	if tor.State != store.StateQueued {
		t.Errorf("state = %q, want QUEUED", tor.State)
	}
	if tor.Category != "tv-sonarr" {
		t.Errorf("category = %q", tor.Category)
	}
	if tor.SavePath != "/downloads/tv-sonarr" {
		t.Errorf("save_path = %q, want /downloads/tv-sonarr", tor.SavePath)
	}
	if tor.Name != "My.Show.S01E01" {
		t.Errorf("name = %q", tor.Name)
	}

	// The category should have been auto-created.
	if c, ok, _ := st.GetCategory(context.Background(), "tv-sonarr"); !ok || c.SavePath != "/downloads/tv-sonarr" {
		t.Errorf("category not created: %+v ok=%v", c, ok)
	}
}

func TestAddMagnetIdempotent(t *testing.T) {
	srv, st := newTestServer(t)
	magnet := "magnet:?xt=urn:btih:" + testHash
	v := url.Values{"urls": {magnet}, "category": {"tv-sonarr"}}

	postForm(t, srv.URL+"/api/v2/torrents/add", v)
	postForm(t, srv.URL+"/api/v2/torrents/add", v)

	torrents, err := st.ListTorrents(context.Background(), store.TorrentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 1 {
		t.Errorf("got %d torrents, want 1 (idempotent)", len(torrents))
	}
}

func TestAddTorrentFileMultipart(t *testing.T) {
	srv, st := newTestServer(t)
	data, wantHash := miniTorrent(t, "file.mkv")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("category", "radarr")
	fw, _ := mw.CreateFormFile("torrents", "x.torrent")
	_, _ = fw.Write(data)
	mw.Close()

	resp, err := http.Post(srv.URL+"/api/v2/torrents/add", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("post multipart: %v", err)
	}
	resp.Body.Close()

	if _, ok, _ := st.GetTorrent(context.Background(), wantHash); !ok {
		t.Fatalf("torrent file not persisted (hash %s)", wantHash)
	}
}

func TestAddNoValidInputFails(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := postForm(t, srv.URL+"/api/v2/torrents/add", url.Values{"urls": {"not-a-magnet"}})
	if strings.TrimSpace(body) != "Fails." {
		t.Errorf("body = %q, want Fails.", body)
	}
}

func TestCreateAndListCategory(t *testing.T) {
	srv, _ := newTestServer(t)
	postForm(t, srv.URL+"/api/v2/torrents/createCategory", url.Values{
		"category": {"tv-sonarr"}, "savePath": {"/custom/tv"},
	})
	_, body := get(t, srv, "/api/v2/torrents/categories")
	var cats map[string]map[string]string
	_ = json.Unmarshal([]byte(body), &cats)
	if cats["tv-sonarr"]["savePath"] != "/custom/tv" {
		t.Errorf("savePath = %v", cats["tv-sonarr"])
	}
}

// miniTorrent bencodes a minimal single-file torrent and returns the bytes plus
// its expected v1 infohash.
func miniTorrent(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	info := map[string]any{
		"name":         name,
		"length":       int64(3),
		"piece length": int64(16384),
		"pieces":       string(make([]byte, 20)),
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	sum := sha1.Sum(infoBytes)
	data, err := bencode.Marshal(map[string]any{
		"announce": "http://tracker.example/announce",
		"info":     bencode.Bytes(infoBytes),
	})
	if err != nil {
		t.Fatalf("marshal torrent: %v", err)
	}
	return data, hex.EncodeToString(sum[:])
}

func TestDeleteRemovesRows(t *testing.T) {
	srv, st := newTestServer(t)
	postForm(t, srv.URL+"/api/v2/torrents/add", url.Values{"urls": {"magnet:?xt=urn:btih:" + testHash}})
	postForm(t, srv.URL+"/api/v2/torrents/delete", url.Values{"hashes": {testHash}, "deleteFiles": {"true"}})

	if _, ok, _ := st.GetTorrent(context.Background(), testHash); ok {
		t.Error("torrent still present after delete")
	}
}
