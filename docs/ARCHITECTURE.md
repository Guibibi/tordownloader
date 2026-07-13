# tordownloader — Architecture

## 1. Overview

tordownloader is a single Go process. It exposes an HTTP server that **emulates the
qBittorrent WebUI API v2** (the consumer side: Sonarr/Radarr), and runs background workers
that drive the **TorBox API** (the producer side) and download finished files to disk.

```
                         ┌──────────────────────── tordownloader ────────────────────────┐
                         │                                                                │
 Sonarr/Radarr ──HTTP──▶ │  qbit handlers ──▶ engine (queue + reconciler + state machine) │
   (qBit API)            │       │                    │            │                       │
                         │       ▼                    ▼            ▼                       │
                         │   store (SQLite) ◀──── torbox client   downloader ──▶ disk      │
                         │                              │            │                      │
                         └──────────────────────────────┼────────────┼──────────────────────┘
                                                         ▼            ▼
                                                     TorBox API   CDN (requestdl URLs)
```

## 2. Components / package layout

```
cmd/tordownloader/main.go      Wires config → store → torbox client → engine → http server.
internal/config/               Load YAML + env; validation; defaults.
internal/qbit/                 HTTP handlers emulating the qBittorrent WebUI API v2.
                               Translates qBit requests ↔ engine/store; renders qBit JSON.
internal/torbox/               Thin TorBox API client (createtorrent, mylist, checkcached,
                               requestdl, controltorrent). Handles auth, rate-limit backoff.
internal/torrentmeta/          Infohash extraction from magnet + .torrent (bencode/SHA1).
internal/store/                SQLite persistence (torrents, files, categories). Pure-Go driver.
internal/engine/               Orchestration: submission queue (slot gating), reconciler
                               (polls TorBox), state machine, fail-fast timer, cleanup.
internal/downloader/           Per-file HTTP downloads with resume (Range), atomic move,
                               bounded concurrency.
docs/                          This documentation.
```

Dependency direction: `qbit` and `engine` depend on `store` and `torbox`; nothing depends on
`qbit`. The engine is the single owner of state transitions; handlers enqueue intents and read
state, they don't mutate torrent lifecycle directly.

## 3. Internal state machine

Each torrent has one **internal state**. The qBittorrent state reported to Sonarr is *derived*
from it (see API_REFERENCE.md §3 for the exact mapping).

```
            add
             │
             ▼
        ┌─────────┐   slot free + submitted to TorBox    ┌──────────────┐
        │ QUEUED  │ ───────────────────────────────────▶ │ TORBOX_ACTIVE│
        └─────────┘                                       └──────────────┘
             │ (waiting for 1 of 3 slots)                   │        │
             │                                              │        │ stall (no progress)
             │                              download_present│        ▼ / TorBox error / >200GB
             │                                              ▼     ┌────────┐
             │                                       ┌─────────────┐│ ERROR │
             │                                       │ LOCAL_QUEUED│└────────┘
             │                                       └─────────────┘     ▲
             │                                              │            │ download fails
             │                                              ▼            │ (after retries)
             │                                       ┌──────────────┐    │
             │                                       │LOCAL_DOWNLOAD│────┘
             │                                       └──────────────┘
             │                                              │ all files on disk
             │                                              ▼
             │                                        ┌──────────┐
             └───────── delete ◀──────────────────────│ COMPLETE │
                       (any state)                    └──────────┘
```

- **QUEUED**: accepted from Sonarr, waiting for a TorBox slot. Reported as in-progress
  (e.g. `stalledDL`/`metaDL`). The stall clock is **not** running here.
- **TORBOX_ACTIVE**: submitted via `createtorrent`; TorBox is fetching/caching. The stall
  detector runs (fail only on no-progress, never for slowness) with tiered patience: a
  fetch stalled at 0% is failed after `stall_timeout` (default 20m — abandoning it costs
  nothing and lets Sonarr re-grab sooner), one with real progress gets
  `progress_stall_timeout` (default 2h — thin swarms lose their seeds and recover on a
  scale of hours; killing a partial fetch blacklists a release that would likely finish),
  and a cached release gets `cached_stall_timeout` (default 30m). While stalled, TorBox is
  nudged with a `reannounce` every `reannounce_interval` (default 5m) so returning seeds
  are picked up promptly. Progress = TorBox progress mapped to the first half of the bar.
- **LOCAL_QUEUED / LOCAL_DOWNLOAD**: TorBox has the files; we are downloading them to disk.
  Progress = second half of the bar.
- **COMPLETE**: all files on disk; reported as `pausedUP` with a valid `content_path`.
- **ERROR**: reported as `error` so Sonarr blacklists and grabs another release.

## 4. Engine loops

- **Submission gate**: a worker pops QUEUED torrents and calls `createtorrent` while the
  account has a free concurrent slot. A torrent occupies a slot only while **`active`**
  (caching/downloading, or seeding); with seeding disabled a **cached** torrent is
  inactive and frees its slot, so library size is unbounded — only concurrent caching is
  capped (3 on Essential). ⚠️ The implementation currently gates on `CountOnTorBox`
  (every row still holding a `torbox_id`/`torbox_queued_id`), which **over-counts** cached
  `LOCAL_*`/un-reaped COMPLETE rows that hold no real slot. Safe but conservative (it can
  only under-submit), and slated to be relaxed to count active/caching torrents only.
  Gating below the cap also keeps `createtorrent` out of TorBox's own queue; our QUEUED
  state is the single queue. On rate-limit, back off and retry (don't fail). On TorBox
  rejection (e.g. `DOWNLOAD_TOO_LARGE`), → ERROR.
- **Reconciler**: every `poll_interval` (default ~10s), reads TorBox `mylist` once and (a)
  **reaps** any COMPLETE/ERROR torrent still on TorBox — deletes it and clears its refs to
  keep `mylist` tidy (hygiene; with seeding off a cached torrent is already inactive and
  holds no slot, so this isn't slot recovery) — then (b) updates each
  TORBOX_ACTIVE torrent's `torbox_state`, `progress`, `download_present`, file list, driving
  TORBOX_ACTIVE → LOCAL_QUEUED and failing torrents that stall (no forward progress for the
  tier's grace — `stall_timeout` at 0%, `progress_stall_timeout` once progressed,
  `cached_stall_timeout` for cached) or exceed the optional absolute `timeout` cap; stalled
  fetches get a `reannounce` nudge every `reannounce_interval` while they wait.
- **Downloader**: pulls LOCAL_QUEUED torrents, enumerates files, requests `requestdl` per file,
  downloads with bounded concurrency into an incomplete dir, atomically moves into the save
  path, then → COMPLETE. Re-requests expired links; resumes partial files via Range on restart.
- **Cleanup**: on `torrents/delete`, cancels in-flight downloads, calls `controltorrent` delete
  on TorBox, removes local files, deletes the DB rows.

## 5. Data model (SQLite)

```sql
torrents(
  id            INTEGER PRIMARY KEY,
  infohash      TEXT UNIQUE NOT NULL,   -- lowercase 40-char hex; the key Sonarr tracks
  torbox_id     INTEGER,                -- TorBox's torrent id (operational handle)
  name          TEXT,
  category      TEXT,
  save_path     TEXT,                   -- <root>/<category>
  content_path  TEXT,                   -- save_path/name (multi-file) or save_path/file
  size          INTEGER,
  magnet        TEXT,                   -- original magnet URI, for (re)submission to TorBox
  source_blob   BLOB,                   -- .torrent bytes when added by file (else NULL)
  state         TEXT,                   -- internal state enum (QUEUED, TORBOX_ACTIVE, ...)
  torbox_state  TEXT,
  torbox_progress REAL,                 -- 0..1
  local_progress  REAL,                 -- 0..1
  dlspeed       INTEGER,
  error         TEXT,
  added_on      INTEGER,                -- unix
  active_since  INTEGER,                -- when it entered TORBOX_ACTIVE
  progress_at   INTEGER,                -- last time TorBox progress advanced (stall clock)
  completed_on  INTEGER,
  created_at    INTEGER,
  updated_at    INTEGER
);

files(
  id             INTEGER PRIMARY KEY,
  torrent_id     INTEGER REFERENCES torrents(id) ON DELETE CASCADE,
  torbox_file_id INTEGER,
  rel_path       TEXT,                  -- path within the torrent (preserves folders)
  short_name     TEXT,
  size           INTEGER,
  downloaded     INTEGER DEFAULT 0,
  done           INTEGER DEFAULT 0
);

categories(
  name      TEXT PRIMARY KEY,
  save_path TEXT
);
```

## 6. Path construction

- `save_path` = category's save path, default `<download_root>/<category>`.
- Each file is written at `<save_path>/<rel_path>`, where `rel_path` is the TorBox file `name`
  (the full path **within** the torrent, including its top folder — verified in API_REFERENCE §5;
  `short_name` is just the basename). So a season pack lands at `<save_path>/<Show.S01>/<ep>.mkv`.
- `content_path` is what Sonarr imports: the single top-level entry under `save_path` — the
  torrent's folder for a multi-file release, or the file itself for a single-file release. It is
  computed from the actual content after download (`downloader.Finalize`), not guessed from the
  name; the reconciler sets a best-effort value earlier for display.
- Download into a per-torrent staging tree `<save_path>/<incomplete_subdir>/<infohash>/...` first,
  then atomically rename each top-level entry into `save_path` once every file is complete and
  size-verified, so Sonarr never imports partial content. Staging lives under `save_path` to keep
  the rename on one filesystem (atomic).

## 7. TorBox client notes

- Bearer token (API key) for `createtorrent`, `mylist`, `checkcached`, `controltorrent`.
- `requestdl` takes the token as a **query parameter** and returns a time-limited CDN URL
  (valid ~3h). We download that URL directly (not via the bearer-auth client).
- Respect rate limits: `createtorrent` 60/hour; others 300/min. Centralise backoff in the client.
- Evaluate the official `torbox-sdk-go` vs a hand-rolled client during M1; lean hand-rolled for
  control over `requestdl` and rate-limit handling, use the SDK if it's ergonomic.

## 8. Tech choices (proposed)

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.26 | User's stack; TorBox has a Go SDK. |
| HTTP router | stdlib `net/http` + `http.ServeMux` (or `chi`) | Small surface; few routes. |
| SQLite | `modernc.org/sqlite` (pure Go) | No cgo → trivial static Docker builds for Unraid. |
| Bencode/infohash | `github.com/anacrolix/torrent/metainfo` | Battle-tested infohash computation. |
| Config | env + small YAML loader (`gopkg.in/yaml.v3`) | Headless, simple. |
| Logging | stdlib `log/slog` | Structured, no deps. |
| Container | multi-stage → distroless/alpine; PUID/PGID entrypoint | Unraid file-ownership convention. |

## 9. Failure handling summary

| Situation | Behavior |
|---|---|
| Stalled while active (no forward progress) | → ERROR after the tier's grace: `stall_timeout` (default 20m) at 0%, `progress_stall_timeout` (default 2h) once the fetch has real progress, `cached_stall_timeout` (default 30m) for cached releases. Sonarr blacklists, re-grabs. Slow-but-moving fetches are not failed. While stalled, TorBox gets a `reannounce` nudge every `reannounce_interval` (default 5m). Any failure of a submitted torrent also best-effort deletes it from TorBox so failed grabs don't pile up. |
| Exceeds optional absolute `timeout` cap (disabled by default) | → ERROR. |
| TorBox rejects >200GB | → ERROR immediately. |
| `createtorrent` rate-limited (429) | Pause submissions for a cooldown (hourly quota), stay QUEUED. |
| TorBox queues the submission (queued id) | Shouldn't happen — we gate on real occupancy. If it does (e.g. an orphan torrent added outside the app holds a slot), cancel the queued entry via `controlqueued` and leave the torrent in our own QUEUED to retry. We never adopt TorBox's queue. |
| Completed/failed torrent still on TorBox (missed delete: old build, crash, transient error) | Reaper deletes it on the next reconcile pass (hygiene). With seeding disabled a cached torrent is already inactive and holds no slot, so a missed delete clutters `mylist` but does **not** starve the queue. |
| `requestdl` link expired mid-download | Re-request and resume. |
| Container restart | Reconcile from SQLite + `mylist`; resume partial files via Range. |
| Torrent vanished on TorBox | → ERROR, but only after a grace window and a direct id lookup confirms it's really gone (so a lagging/paged mylist doesn't kill a live torrent). |
| Sonarr deletes mid-download | Cancel, TorBox delete, remove local files. |
