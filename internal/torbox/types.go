package torbox

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// IsNotFound reports whether TorBox answered that the requested item does not
// exist: mylist?id= for a deleted torrent returns HTTP 404 (ITEM_NOT_FOUND). A
// 404 for the endpoint itself (ENDPOINT_NOT_FOUND, e.g. after an API path
// change) says nothing about the item and does not count.
func (e *APIError) IsNotFound() bool {
	if e.Code == "ENDPOINT_NOT_FOUND" {
		return false
	}
	return e.StatusCode == http.StatusNotFound || e.Code == "ITEM_NOT_FOUND"
}

// IsDuplicate reports whether TorBox refused a create because the item is
// already on the account.
func (e *APIError) IsDuplicate() bool { return e.Code == "DUPLICATE_ITEM" }

// transientCodes are TorBox error codes that describe the account or the
// service, not the release: auth problems, plan/rate limits, and server-side
// failures. Retrying later can succeed, so a submission rejected with one of
// these must not be failed (failing pushes a blocklist to Sonarr/Radarr).
var transientCodes = map[string]bool{
	"NO_AUTH":                    true,
	"BAD_TOKEN":                  true,
	"AUTH_ERROR":                 true,
	"ACTIVE_LIMIT":               true,
	"COOLDOWN_LIMIT":             true,
	"MONTHLY_LIMIT":              true,
	"RATE_LIMIT":                 true,
	"NO_SERVERS_AVAILABLE_ERROR": true,
	"DOWNLOAD_SERVER_ERROR":      true,
	"DATABASE_ERROR":             true,
	"UNKNOWN_ERROR":              true,
	"ENDPOINT_NOT_FOUND":         true,
	"VENDOR_DISABLED":            true,
	"DEPRECATED":                 true,
}

// IsTransient reports whether the error is about TorBox or the account rather
// than the request itself — a rate limit (429), an auth failure (401/403), a
// server error (5xx), or one of transientCodes — so the same request may succeed
// later. Anything else (e.g. DOWNLOAD_TOO_LARGE, a malformed torrent) is a
// verdict on the release.
func (e *APIError) IsTransient() bool {
	switch {
	case e.StatusCode == http.StatusTooManyRequests,
		e.StatusCode == http.StatusUnauthorized,
		e.StatusCode == http.StatusForbidden,
		e.StatusCode >= 500:
		return true
	}
	return transientCodes[e.Code]
}

// Seeding preferences for CreateTorrentRequest.Seed (TorBox's `seed` field).
const (
	SeedAuto   = 1 // follow the account setting
	SeedAlways = 2
	SeedNever  = 3
)

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
	Seed     int   // SeedAuto/SeedAlways/SeedNever; 0 = omit (account default)
	AllowZip *bool // nil = omit
	AsQueued bool
}

// CreateTorrentResult is the data returned by /torrents/createtorrent: a
// torrent_id when TorBox starts the download, or a queued_id when it queues it
// instead. Field names match TorBox's official SDK (see docs/API_REFERENCE.md).
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
