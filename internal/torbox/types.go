package torbox

import (
	"encoding/json"
	"fmt"
)

// Envelope is the standard TorBox JSON response wrapper:
//
//	{ "success": bool, "error": <string|false|null>, "detail": string, "data": <any> }
type Envelope struct {
	Success bool            `json:"success"`
	Error   json.RawMessage `json:"error"`
	Detail  string          `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

// errorCode extracts the machine error code when `error` is a JSON string
// (e.g. "DOWNLOAD_TOO_LARGE"); returns "" when it's false/null/absent.
func (e Envelope) errorCode() string {
	if len(e.Error) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.Error, &s); err == nil {
		return s
	}
	return ""
}

// APIError is returned when TorBox reports a non-success response.
type APIError struct {
	StatusCode int
	Code       string // machine code from the `error` field, if any
	Detail     string // human-readable message from `detail`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("torbox: %s (%s) [http %d]", e.Detail, e.Code, e.StatusCode)
	}
	return fmt.Sprintf("torbox: %s [http %d]", e.Detail, e.StatusCode)
}

// IsTooLarge reports whether TorBox rejected the download for exceeding the
// plan's per-download size cap (200GB on Essential). The exact error code is
// provisional pending live verification (see docs/API_REFERENCE.md).
func (e *APIError) IsTooLarge() bool { return e.Code == "DOWNLOAD_TOO_LARGE" }

// Operation is a torrent control operation for /torrents/controltorrent.
type Operation string

const (
	OpReannounce Operation = "reannounce"
	OpDelete     Operation = "delete"
	OpResume     Operation = "resume"
)

// Torrent is a TorBox torrent as returned by /torrents/mylist.
//
// Field names verified against the live API on 2026-06-14 (see
// docs/API_REFERENCE.md §5). The API returns more fields than we model; we keep
// the ones the engine needs plus a couple of conveniences.
type Torrent struct {
	ID               int           `json:"id"`
	Hash             string        `json:"hash"`
	Name             string        `json:"name"`
	Size             int64         `json:"size"`
	DownloadState    string        `json:"download_state"` // e.g. downloading, cached, stalled, metaDL, completed
	DownloadFinished bool          `json:"download_finished"`
	DownloadPresent  bool          `json:"download_present"` // files are available to download — our trigger
	Cached           bool          `json:"cached"`
	Progress         float64       `json:"progress"`
	Active           bool          `json:"active"`
	DownloadSpeed    int64         `json:"download_speed"`
	ETA              int64         `json:"eta"`
	Seeds            int           `json:"seeds"`
	Peers            int           `json:"peers"`
	DownloadPath     string        `json:"download_path"`
	Files            []TorrentFile `json:"files"`
	CreatedAt        string        `json:"created_at"`
	UpdatedAt        string        `json:"updated_at"`
}

// TorrentFile is one file within a TorBox torrent. Verified 2026-06-14:
// `ID` is zero-indexed (used as file_id for requestdl); `Name` is the full path
// within the torrent (preserves folders); `ShortName` is the basename.
type TorrentFile struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	Size      int64  `json:"size"`
}

// CreateTorrentRequest is the input to CreateTorrent. Provide either Magnet or
// File (with FileName). Zero-valued optional fields are omitted from the request.
type CreateTorrentRequest struct {
	Magnet   string
	File     []byte
	FileName string
	Name     string
	Seed     int   // seeding preference; 0 = omit (use TorBox default)
	AllowZip *bool // nil = omit
	AsQueued bool
}

// CreateTorrentResult is the data returned by /torrents/createtorrent.
// PROVISIONAL shape — NOT yet verified against the live API (no write was
// performed during the read-only M1 verification). Confirm before relying on it.
type CreateTorrentResult struct {
	TorrentID *int   `json:"torrent_id"`
	QueuedID  *int   `json:"queued_id"`
	Hash      string `json:"hash"`
	Name      string `json:"name"`
}

// RequestDLParams is the input to RequestDL. FileID nil requests the whole
// torrent (typically with ZipLink); set FileID for a single-file link.
type RequestDLParams struct {
	TorrentID int
	FileID    *int
	ZipLink   bool
	UserIP    string
}

// CachedInfo is one entry from /torrents/checkcached (format=object), which
// returns an object keyed by infohash. Verified 2026-06-14.
type CachedInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}
