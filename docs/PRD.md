# tordownloader — Product Requirements (PRD)

## 1. Problem

The user runs Sonarr/Radarr/Prowlarr on an Unraid host and wants to use **TorBox** (debrid)
as the actual downloader, instead of a local BitTorrent client. Existing tools either require
a fuse/WebDAV **mount** of the debrid (rclone/zurg/decypharr streaming model) or are tied to
other debrid providers. The user explicitly does **not** want a mount — they want releases
sent to TorBox, and the finished files **downloaded to local disk** into category folders,
so Sonarr/Radarr import them exactly like a normal qBittorrent setup.

## 2. Goal

A headless Go service that **emulates the qBittorrent WebUI API v2** so Sonarr/Radarr treat
it as an ordinary download client, while internally it proxies to TorBox and materialises
completed files on local disk.

## 3. Users / context

- Single user, single Unraid host (home media stack).
- Sonarr, Radarr, Prowlarr already running as Docker containers on the same host.
- The download folder is a shared volume, mapped **identically** across all containers
  (so reported paths need no Remote Path Mapping).
- TorBox **Essential** plan: 3 concurrent slots, 200GB max per download, unlimited downloads,
  1Gbps, 24h seeding, API access, **no Usenet**.

## 4. Scope

### In scope (v1)
- Emulate the qBittorrent WebUI API v2 subset that Sonarr/Radarr use (see API_REFERENCE.md).
- Accept magnet links and `.torrent` file uploads; extract the correct infohash.
- Submit to TorBox, respecting the 3 concurrent slots via an internal queue.
- Poll TorBox for status; map TorBox state/progress to qBittorrent states.
- On TorBox completion, download **each file individually** via `requestdl`, preserving the
  torrent's folder structure, into `<root>/<category>/<name>/...`.
- Report "complete" to Sonarr/Radarr only after files are fully on local disk.
- Categories → folders mapping, with create/list/set category support.
- On Sonarr/Radarr delete: remove the torrent from TorBox **and** delete the local source.
- Persist all state in SQLite; survive restarts (resume in-progress downloads).
- Headless config (YAML + env), structured logs.
- Ship as a single Docker image suitable for Unraid (PUID/PGID file ownership).

### Out of scope (v1)
- Usenet (TorBox usenet would require SABnzbd API emulation; the user's plan has no Usenet).
- Web downloads / debrid link downloads.
- Web UI / dashboard (logs only for now; keep internals clean for a possible later UI).
- Mount-based streaming / symlinks (explicitly not wanted).
- Multi-user / auth on the qBittorrent API (LAN trust; no auth required).
- Seeding/ratio management (we delete on import; TorBox handles its own 24h seeding).

## 5. Functional requirements

| # | Requirement |
|---|---|
| FR1 | Expose qBittorrent WebUI API v2 endpoints required by Sonarr/Radarr (login, app/version, app/webapiVersion, app/preferences, torrents/{info,add,delete,files,properties,categories,createCategory,setCategory,pause,resume,topPrio}, transfer/info). |
| FR2 | `torrents/add` accepts `urls` (newline-separated magnet/http links) and multipart `.torrent` files, plus `category` and `paused` params; returns `Ok.` |
| FR3 | Extract the v1 infohash from the magnet (`xt=urn:btih:`) or by bencode-hashing the `.torrent` info dict; normalise to lowercase 40-char hex. All tracking is keyed on this hash so it matches what Sonarr tracks. |
| FR4 | Forward the release to TorBox `createtorrent`; store mapping infohash↔torbox_id↔category. |
| FR5 | Limit concurrent active TorBox downloads to a configurable max (default 3). Extra releases wait in a local queue and report an in-progress state to Sonarr. |
| FR6 | On add, optionally check TorBox cached availability (informational). Report qBittorrent state `error` only when a fetching torrent **stalls** — no bytes moving and progress not climbing for `failure.stall_timeout` (**default 10m**, measured while TorBox-active, not while waiting in our local queue). A slow but still-progressing fetch is never failed for being slow. An optional absolute `failure.timeout` cap (disabled by default) bounds how long a perpetually-slow torrent may hold a slot. |
| FR7 | Poll TorBox `mylist` to update each tracked torrent's state and progress. |
| FR8 | Map TorBox state + local download progress to a qBittorrent state and a blended progress value (TorBox half + local-download half). Report `error` on failures (including TorBox rejecting >200GB downloads). |
| FR9 | When TorBox reports the torrent present/finished, download each file via `requestdl` (per-file, preserve folder tree) into the category save path, using a temp/incomplete dir and atomic move so Sonarr never sees partial files. |
| FR10 | Report a completed state (`pausedUP`) and a valid `content_path` only after all files are on disk. |
| FR11 | On `torrents/delete`, cancel any in-flight download, delete the torrent from TorBox (`controltorrent` delete), and remove the local source files. |
| FR12 | Persist torrents, files, and categories in SQLite. On restart, reconcile against TorBox and resume incomplete file downloads (HTTP Range). |
| FR13 | Re-request `requestdl` links if they expire (~3h validity) mid-download. |
| FR14 | Configurable via YAML/env: TorBox API key, listen address, download root, parallelism, slot limit, stall timeout, optional absolute timeout, poll interval, log level. |

## 6. Non-functional requirements

- **Reliability**: survive restarts without losing track of in-progress downloads or
  re-grabbing completed ones. Idempotent add (same infohash → same torrent).
- **Rate-limit safety**: respect TorBox limits — `createtorrent` 60/hour, general 300/min;
  back off and retry rather than fail when rate-limited.
- **Resource use**: streaming file downloads (no full-file buffering in memory); bounded
  download concurrency.
- **Portability**: pure-Go build (no cgo) so the Docker image is easy to build/run on Unraid.
- **Observability**: structured logs (slog) covering add, queue, TorBox state transitions,
  download progress/errors, and cleanup.

## 7. Key risks / decisions

- **200GB cap vs whole-series packs**: a full-series pack of a long show can exceed TorBox's
  200GB per-download cap. Expected behavior: TorBox rejects it, we report `error`, Sonarr
  blacklists that release and falls back to a season/smaller pack. This is correct, not a bug.
- **Infohash correctness**: if our reported hash doesn't exactly match what Sonarr computed,
  Sonarr loses track of the download. v2/hybrid torrents complicate this; v1 infohash is the
  primary case and what we key on.
- **Path identity**: relies on the download folder being mounted at the same path inside the
  tordownloader and Sonarr/Radarr containers.

## 8. Success criteria

- Adding a show in Sonarr results in the season pack being grabbed, sent to TorBox,
  downloaded to `/downloads/<category>/...`, imported by Sonarr, then cleaned up — with no
  manual intervention.
- A dead/unseeded release that makes no progress fails after the stall window (~10m) and
  Sonarr automatically grabs an alternative; a slow-but-progressing fetch is left to finish.
- The service recovers cleanly from a container restart mid-download.
