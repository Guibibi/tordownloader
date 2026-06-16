# tordownloader — API Reference

This is the contract on both sides: the qBittorrent WebUI API v2 endpoints we **emulate** for
Sonarr/Radarr, the TorBox endpoints we **consume**, and the state mapping between them.

References: rdt-client emulates qBittorrent WebUI API v4.3.2 / Web API 2.7. We report the same
version strings for compatibility.

---

## 1. qBittorrent WebUI API v2 — endpoints we emulate

All under `/api/v2`. Methods accept GET and/or POST as qBittorrent does. No auth required
(LAN trust), but we still implement `auth/login` because Sonarr calls it.

### Auth & app
| Endpoint | Behavior |
|---|---|
| `POST /auth/login` | Accept any username/password; return body `Ok.` and set a `SID` cookie. |
| `POST /auth/logout` | Return 200. |
| `GET /app/version` | Return `v4.3.2`. |
| `GET /app/webapiVersion` | Return `2.7`. |
| `GET /app/buildInfo` | Return a static JSON build-info blob (optional). |
| `GET /app/preferences` | Return JSON with real `save_path` (download root) + mostly static fields. |
| `GET /app/setPreferences` | Stub, return 200. |

### Torrents — read
| Endpoint | Behavior |
|---|---|
| `GET /torrents/info` | Return an array of torrent objects (see §2). Supports `category` and `hashes` filters. **This is the endpoint Sonarr polls** to track state/progress and find completed content. |
| `GET /torrents/properties` | Per-`hash` details: `save_path`, `total_size`, `total_downloaded`, timestamps, speeds. |
| `GET /torrents/files` | Per-`hash` file list: `name` (rel path), `size`, `progress`, `priority`, `index`. |

### Torrents — write
| Endpoint | Params | Behavior |
|---|---|---|
| `POST /torrents/add` | `urls` (newline-separated magnet/http), multipart `torrents` file(s), `category`, `paused` | Create torrents; return `Ok.` Extract infohash, persist, enqueue. Idempotent on infohash. |
| `POST /torrents/delete` | `hashes` (pipe-separated or `all`), `deleteFiles` | Cancel, TorBox-delete, remove local files, drop DB rows. |
| `POST /torrents/pause` / `resume` / `setForceStart` / `topPrio` | `hashes` | Mostly stubs returning 200 (no real pause concept on debrid). `topPrio` may bump queue order. |
| `POST /torrents/setShareLimits` | — | Stub 200. |

### Categories
| Endpoint | Params | Behavior |
|---|---|---|
| `GET /torrents/categories` | — | Return `{ "<name>": {"name","savePath"}, ... }`. |
| `POST /torrents/createCategory` | `category`, `savePath` | Persist category → save path (default `<root>/<category>`). |
| `POST /torrents/setCategory` | `hashes`, `category` | Reassign category (and recompute save/content paths). |
| `POST /torrents/removeCategories` | `categories` | Remove from settings. |

### Transfer / sync
| Endpoint | Behavior |
|---|---|
| `GET /transfer/info` | `connection_status: "connected"`, aggregate `dl_info_speed`, etc. |
| `GET /sync/maindata` | Optional snapshot (`full_update: true`); Sonarr mainly uses `torrents/info`. |

---

## 2. The qBittorrent torrent object (what `torrents/info` returns)

Fields Sonarr/Radarr actually read — populate all of these:

| Field | Source |
|---|---|
| `hash` | our infohash (lowercase 40-hex) — must match what Sonarr tracks |
| `name` | torrent name |
| `size` | total bytes |
| `progress` | `0.5*torbox_progress + 0.5*local_progress` (0..1) |
| `dlspeed` | local download speed (bytes/s) while downloading; else 0 |
| `eta` | estimated seconds remaining (or 8640000 when unknown) |
| `state` | see §3 |
| `category` | category name |
| `save_path` | `<root>/<category>` |
| `content_path` | full path to content (folder or single file) |
| `completed` | bytes on disk |
| `amount_left` | `size - completed` |
| `completion_on` | unix ts when COMPLETE (else 0) |
| `added_on` | unix ts when added |
| `ratio`, `seeding_time` | 0 (no seeding on our side) |

## 3. State mapping (TorBox + local → qBittorrent `state`)

Sonarr decides "ready to import" from the **state**, not just progress. "UP" states mean
done/seeding → importable. So COMPLETE must report an "UP" state (`pausedUP`).

| Internal state | Condition | qBittorrent `state` | Sonarr sees |
|---|---|---|---|
| QUEUED | waiting for a TorBox slot | `stalledDL` (or `metaDL`) | downloading, waits |
| TORBOX_ACTIVE | TorBox fetching, has seeds/progress | `downloading` | downloading |
| TORBOX_ACTIVE | TorBox fetching, no seeds | `stalledDL` | downloading |
| LOCAL_QUEUED / LOCAL_DOWNLOAD | pulling files to disk | `downloading` | downloading |
| COMPLETE | all files on disk | `pausedUP` | **done → import** |
| ERROR | stall (no progress) / TorBox fail / >200GB / dl fail | `error` | **failed → blacklist & re-grab** |

> A TORBOX_ACTIVE torrent is failed only when it **stalls** — no bytes moving and
> progress not climbing — for `failure.stall_timeout` (default 10m). It is never
> failed just for being slow: a download still moving bytes keeps resetting the
> stall clock (tracked via `torrents.progress_at`), so a legitimately slow,
> uncached fetch runs as long as it needs. `failure.timeout` is an optional
> absolute cap from when it became active, **disabled by default** (set it to bound
> how long a perpetually-slow torrent may hold a scarce TorBox slot). Set
> `stall_timeout` ≤ 0 to disable stall detection.

> Never report `pausedDL` for a finished item — Sonarr treats `*DL` states as not-done.
> Use `pausedUP` (an "UP" state) to signal completion.

---

## 4. TorBox API — endpoints we consume

Base `https://api.torbox.app`, version `v1`. Bearer token (API key) unless noted.
Rate limits: most 300/min; **`createtorrent` 60/hour**.

| Endpoint | Method | Use |
|---|---|---|
| `/v1/api/torrents/createtorrent` | POST (multipart) | Submit a release. Params: `magnet` **or** `file`, `seed`, `allow_zip`, `name`, `as_queued`. Returns `data.torrent_id` (+ `hash`). ⚠️ Response shape **not yet verified** (read-only M1 run did no writes). |
| `/v1/api/torrents/checkcached` | GET | Cached availability. `hash` (comma list, ~100 max), `format=object`, `list_files`. **Verified 2026-06-14:** `data` is an object keyed by infohash → `{name, size, hash}` (empty array when none cached). |
| `/v1/api/torrents/mylist` | GET | List/poll torrents. Params: `bypass_cache`, `id`, `offset`, `limit` (default 1000). Returns torrent objects (see §5). **Verified 2026-06-14.** |
| `/v1/api/torrents/requestdl` | GET | Get a time-limited CDN download URL. Params: `token` (API key, as query param), `torrent_id`, `file_id`, `zip_link`, `user_ip`, `redirect`. Link valid ~3h. **Per-file**: one call per `file_id`. **Verified 2026-06-14:** `data` is a plain string URL like `https://<store>.tb-cdn.io/dld/<uuid>?token=<key>`. |
| `/v1/api/torrents/controltorrent` | POST | `{torrent_id, operation}` where operation ∈ `reannounce|delete|resume`. We use `delete` for cleanup of **active** torrents. |
| `/v1/api/queued/controlqueued` | POST | `{queued_id, operation, type}` (`type="torrent"`). We use `delete` only to **cancel** an unexpected queued submission (we gate on real occupancy and don't adopt TorBox's queue) and to clean up legacy queued rows. A queued download is a separate namespace `controltorrent` won't touch. **Verified live 2026-06-15.** |
| `/v1/api/queued/getqueued` | GET | List queued downloads. Params: `bypass_cache`, `type=torrent`, `limit`. Returns objects with `id` (the `queued_id`), `hash`, `name`, `magnet`, `created_at`. **Verified live 2026-06-15.** ⚠️ The old `/v1/api/torrents/getqueued` is **deprecated** (returns `error: "DEPRECATED"`). |
| `/v1/api/torrents/torrentinfo` | GET/POST | Metadata from DHT by `hash`/`magnet`/`file` (no auth). Optional, for name/file list pre-cache. |

### Standard TorBox response envelope
```json
{ "success": true, "error": null, "detail": "…", "data": { … } }
```
On **success**, observed `error` is `false` (not `null`); on failure it is a machine code
string (e.g. `DOWNLOAD_TOO_LARGE` — provisional, unverified). `detail` is human-readable.
HTTP: 200 ok, 400 bad input, 403 auth, 500 server. Our client treats `error` as a union of
`string|false|null`.

## 5. TorBox torrent object (from `mylist`) — verified 2026-06-14

Confirmed against a live `mylist` response. The API returns more fields than we model; the
ones the engine uses:

| Field | Meaning |
|---|---|
| `id` | TorBox torrent id (our operational handle), e.g. `40262966` |
| `hash` | infohash (matches our v1 infohash, lowercase 40-hex) |
| `name` | torrent name |
| `size` | total bytes |
| `download_state` | observed `cached`; per docs also `downloading`/`uploading`/`stalled`/`paused`/`completed`/`metaDL`/`checkingResumeData` |
| `download_finished` | bool |
| `download_present` | bool — files are available to download from TorBox |
| `cached` | bool — convenience flag (true when state is `cached`) |
| `progress` | 0..1 |
| `active` | bool (consuming a slot) |
| `download_speed`, `eta`, `seeds`, `peers` | live transfer stats |
| `download_path` | TorBox-side folder name (usually the infohash) |
| `files[]` | `{ id, name, short_name, size }` (+ `md5`, `hash`, `s3_path`, `mimetype`, `absolute_path`, `zipped`, ...) |

**Confirmed file details:** `files[].id` is **zero-indexed** (first file is `id:0`) and is the
`file_id` passed to `requestdl`. `files[].name` is the **full path within the torrent**
(e.g. `Show.S01/Show.S01E01.mkv`), so writing it under the save path preserves folder structure.
`files[].short_name` is just the basename.

We treat **`download_present == true`** as the trigger to begin local downloads.

> **Slot semantics.** `active == true` is the only thing that consumes one of the
> account's concurrent slots (downloading/caching, or seeding). A `cached`, non-seeding
> torrent is `active: false` and consumes **no** slot — so with seeding disabled,
> completed torrents free their slot automatically and the library is effectively
> unbounded; only concurrent *caching* is capped (3 on Essential). A cached torrent does
> **not** pin a slot until deleted (an earlier assumption that was wrong).

> Other fields present but unused: `auth_id`, `server`, `magnet`, `ratio`, `upload_speed`,
> `torrent_file`, `expires_at`, `availability`, `total_uploaded`, `total_downloaded`, `owner`,
> `allow_zipped`, `seed_torrent`, `long_term_seeding`, `private`, `cached_at`,
> `alternative_hashes`, `tags`.
