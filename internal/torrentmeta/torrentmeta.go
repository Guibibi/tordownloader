// Package torrentmeta extracts the v1 BitTorrent infohash (and a display name)
// from the inputs Sonarr/Radarr hand us: magnet URIs and .torrent files.
//
// The infohash is normalised to lowercase 40-hex. It MUST match the v1 infohash
// Sonarr computed, or Sonarr loses track of the download (see CLAUDE.md), so we
// rely on anacrolix/torrent/metainfo rather than parsing by hand.
package torrentmeta

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// Meta is the identity extracted from an add input.
type Meta struct {
	InfoHash string // lowercase 40-hex v1 infohash
	Name     string // display name, may be empty
}

// ErrNoV1InfoHash is returned when an input carries no v1 (btih) infohash — e.g.
// a magnet with only a v2 (btmh) hash. TorBox and Sonarr both key on v1, so we
// can't use such an input.
var ErrNoV1InfoHash = errors.New("torrentmeta: no v1 infohash in input")

// FromMagnet parses a magnet URI and returns its v1 infohash and display name.
func FromMagnet(uri string) (Meta, error) {
	m, err := metainfo.ParseMagnetUri(strings.TrimSpace(uri))
	if err != nil {
		return Meta{}, fmt.Errorf("torrentmeta: parse magnet: %w", err)
	}
	if m.InfoHash.IsZero() {
		return Meta{}, ErrNoV1InfoHash
	}
	return Meta{
		InfoHash: strings.ToLower(m.InfoHash.HexString()),
		Name:     m.DisplayName,
	}, nil
}

// FromTorrent parses .torrent file bytes and returns the v1 infohash (SHA1 of
// the bencoded info dict) and the torrent name.
func FromTorrent(data []byte) (Meta, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return Meta{}, fmt.Errorf("torrentmeta: load torrent: %w", err)
	}
	h := mi.HashInfoBytes()
	if h.IsZero() {
		return Meta{}, ErrNoV1InfoHash
	}
	meta := Meta{InfoHash: strings.ToLower(h.HexString())}
	if info, err := mi.UnmarshalInfo(); err == nil {
		meta.Name = info.Name
	}
	return meta, nil
}

// IsMagnet reports whether s looks like a magnet URI.
func IsMagnet(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(s)), "magnet:")
}
