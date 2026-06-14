package store

// Internal torrent states. These are the values persisted in torrents.state and
// are the single source of truth shared across the store, engine, and qbit
// layers. The qBittorrent state reported to Sonarr is *derived* from these (see
// docs/API_REFERENCE.md §3).
const (
	StateQueued       = "QUEUED"        // accepted, waiting for a TorBox slot
	StateTorBoxActive = "TORBOX_ACTIVE" // submitted; TorBox is fetching/caching
	StateLocalQueued  = "LOCAL_QUEUED"  // files present on TorBox, waiting to download
	StateLocalDload   = "LOCAL_DOWNLOAD"
	StateComplete     = "COMPLETE" // all files on disk
	StateError        = "ERROR"    // timeout / TorBox fail / >200GB / dl fail
)
