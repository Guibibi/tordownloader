package torrentmeta

import (
	"crypto/sha1"
	"encoding/hex"
	"testing"

	"github.com/anacrolix/torrent/bencode"
)

func TestFromMagnetHex(t *testing.T) {
	const hash = "C12FE1C06BBA254A9DC9F519B335AA7C1367A88A"
	uri := "magnet:?xt=urn:btih:" + hash + "&dn=Some+Release"
	m, err := FromMagnet(uri)
	if err != nil {
		t.Fatalf("FromMagnet: %v", err)
	}
	if m.InfoHash != "c12fe1c06bba254a9dc9f519b335aa7c1367a88a" {
		t.Errorf("infohash = %q", m.InfoHash)
	}
	if m.Name != "Some Release" {
		t.Errorf("name = %q", m.Name)
	}
}

func TestFromMagnetBase32(t *testing.T) {
	// Base32 of the 20 raw bytes of the hex hash above.
	uri := "magnet:?xt=urn:btih:YEX6DQDLXISUVHOJ6UM3GNNKPQJWPKEK"
	m, err := FromMagnet(uri)
	if err != nil {
		t.Fatalf("FromMagnet base32: %v", err)
	}
	if m.InfoHash != "c12fe1c06bba254a9dc9f519b335aa7c1367a88a" {
		t.Errorf("base32 infohash = %q, want c12fe1c0...", m.InfoHash)
	}
}

func TestFromMagnetInvalid(t *testing.T) {
	if _, err := FromMagnet("not a magnet"); err == nil {
		t.Error("expected error for non-magnet")
	}
}

// buildTorrent bencodes a minimal single-file torrent and returns the bytes
// plus the expected v1 infohash (sha1 of the bencoded info dict).
func buildTorrent(t *testing.T, name string) ([]byte, string) {
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
	wantHash := hex.EncodeToString(sum[:])

	mi := map[string]any{
		"announce": "http://tracker.example/announce",
		"info":     bencode.Bytes(infoBytes),
	}
	data, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal torrent: %v", err)
	}
	return data, wantHash
}

func TestFromTorrent(t *testing.T) {
	data, wantHash := buildTorrent(t, "a.txt")
	m, err := FromTorrent(data)
	if err != nil {
		t.Fatalf("FromTorrent: %v", err)
	}
	if m.InfoHash != wantHash {
		t.Errorf("infohash = %q, want %q", m.InfoHash, wantHash)
	}
	if m.Name != "a.txt" {
		t.Errorf("name = %q, want a.txt", m.Name)
	}
}

func TestFromTorrentGarbage(t *testing.T) {
	if _, err := FromTorrent([]byte("not bencode")); err == nil {
		t.Error("expected error for garbage torrent")
	}
}

func TestIsMagnet(t *testing.T) {
	if !IsMagnet("magnet:?xt=urn:btih:abc") {
		t.Error("expected magnet")
	}
	if IsMagnet("http://example/x.torrent") {
		t.Error("http is not a magnet")
	}
}
