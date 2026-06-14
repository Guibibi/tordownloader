package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// These tests hit the real TorBox API. They are skipped unless TORBOX_API_KEY
// is set, so they never run in CI by default. Read-only tests (mylist,
// checkcached) run with just the key. The write flow (createtorrent → poll →
// requestdl → delete) additionally requires TORBOX_LIVE_MAGNET to be set.

func liveClient(t *testing.T) *Client {
	t.Helper()
	key := os.Getenv("TORBOX_API_KEY")
	if key == "" {
		t.Skip("TORBOX_API_KEY not set; skipping live TorBox test")
	}
	return New(key)
}

func TestLive_MyList(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := c.MyList(ctx, true)
	if err != nil {
		t.Fatalf("MyList: %v", err)
	}
	t.Logf("account has %d torrent(s)", len(list))
	if len(list) > 0 {
		tr := list[0]
		t.Logf("first torrent (typed): id=%d hash=%s state=%q present=%v finished=%v progress=%.3f files=%d",
			tr.ID, tr.Hash, tr.DownloadState, tr.DownloadPresent, tr.DownloadFinished, tr.Progress, len(tr.Files))
		// Dump raw JSON so we can verify field names against types.go.
		t.Logf("first torrent (raw JSON):\n%s", rawFirstTorrent(t, c))
	}
}

func TestLive_CheckCached(t *testing.T) {
	c := liveClient(t)
	hash := os.Getenv("TORBOX_LIVE_HASH")
	if hash == "" {
		t.Skip("TORBOX_LIVE_HASH not set; skipping checkcached probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.CheckCached(ctx, []string{hash}, true)
	if err != nil {
		t.Fatalf("CheckCached: %v", err)
	}
	t.Logf("checkcached(%s): %d cached entr(ies): %+v", hash, len(res), res)
}

func TestLive_RequestDL(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := c.MyList(ctx, true)
	if err != nil {
		t.Fatalf("MyList: %v", err)
	}
	// Pick the first finished torrent with files — read-only: requesting a link
	// does not change account state or consume a slot.
	var tr *Torrent
	for i := range list {
		if list[i].DownloadPresent && len(list[i].Files) > 0 {
			tr = &list[i]
			break
		}
	}
	if tr == nil {
		t.Skip("no present torrent with files on the account; cannot probe requestdl read-only")
	}

	fileID := tr.Files[0].ID
	link, err := c.RequestDL(ctx, RequestDLParams{TorrentID: tr.ID, FileID: &fileID})
	if err != nil {
		t.Fatalf("RequestDL: %v", err)
	}
	t.Logf("requestdl(torrent=%d file=%d %q) -> %s", tr.ID, fileID, tr.Files[0].ShortName, link)
	if link == "" {
		t.Errorf("empty download link")
	}
}

func TestLive_WriteFlow(t *testing.T) {
	c := liveClient(t)
	magnet := os.Getenv("TORBOX_LIVE_MAGNET")
	if magnet == "" {
		t.Skip("TORBOX_LIVE_MAGNET not set; skipping write flow (createtorrent/requestdl/delete)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	created, err := c.CreateTorrent(ctx, CreateTorrentRequest{Magnet: magnet})
	if err != nil {
		t.Fatalf("CreateTorrent: %v", err)
	}
	if created.TorrentID == nil {
		t.Fatalf("createtorrent returned no torrent_id (queued_id=%v) — inspect response shape", created.QueuedID)
	}
	id := *created.TorrentID
	t.Logf("created torrent id=%d hash=%s", id, created.Hash)
	defer func() {
		if err := c.DeleteTorrent(context.Background(), id); err != nil {
			t.Logf("cleanup delete failed: %v", err)
		} else {
			t.Logf("cleaned up torrent id=%d", id)
		}
	}()

	// Poll until the files are present on TorBox.
	var tr *Torrent
	for {
		tr, err = c.GetTorrent(ctx, id, true)
		if err != nil {
			t.Fatalf("GetTorrent: %v", err)
		}
		t.Logf("poll: state=%q present=%v progress=%.3f", tr.DownloadState, tr.DownloadPresent, tr.Progress)
		if tr.DownloadPresent {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for download_present")
		case <-time.After(5 * time.Second):
		}
	}

	if len(tr.Files) == 0 {
		t.Fatalf("present torrent has no files listed")
	}
	fileID := tr.Files[0].ID
	link, err := c.RequestDL(ctx, RequestDLParams{TorrentID: id, FileID: &fileID})
	if err != nil {
		t.Fatalf("RequestDL: %v", err)
	}
	t.Logf("download link for file %d (%s): %s", fileID, tr.Files[0].Name, link)
	if link == "" {
		t.Errorf("empty download link")
	}
}

// rawFirstTorrent fetches /mylist directly and returns the first torrent as
// pretty JSON, for eyeballing real field names against our structs.
func rawFirstTorrent(t *testing.T, c *Client) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.baseURL+"/"+c.apiVersion+"/api/torrents/mylist?limit=1&bypass_cache=true", nil)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "raw fetch error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return string(body)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(env.Data, &arr); err != nil || len(arr) == 0 {
		return string(env.Data)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, arr[0], "", "  "); err != nil {
		return string(arr[0])
	}
	return pretty.String()
}
