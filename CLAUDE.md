# CLAUDE.md — orientation for future sessions

## What this is
**tordownloader**: a Go service that emulates the **qBittorrent WebUI API v2** so
**Sonarr/Radarr** use **TorBox** (debrid) as a download client, then downloads finished files
to local disk. No mount — real downloads. Like rdt-client/decypharr but TorBox-only, Go,
headless, download-to-disk. Runs as one Docker container on the user's **Unraid** host
(Sonarr/Radarr/Prowlarr already there; `/downloads` shared and mapped identically).

## Read first
- `docs/ARCHITECTURE.md` — components, state machine, data model, tech choices
- `docs/API_REFERENCE.md` — qBit endpoints emulated + TorBox endpoints used + **state mapping**

## Project tracking (Linear)
- Milestones map 1:1 to Linear issues in team **GUI** (Guibibi): M0–M8 = **GUI-22 … GUI-30**.
- When a milestone is finished, update Linear with `linear-cli`:
  - mark it done: `linear-cli i update GUI-NN -s Done`
  - add a short summary comment with the commit hash + what shipped:
    `linear-cli cm create GUI-NN -b "Done (commit <sha>). <summary>"`
- Status: GUI-22…GUI-30 (M0–M8) all **Done**. Project is feature-complete through M8.

## Locked decisions (don't re-litigate)
- Torrents only (no Usenet — user's plan has none).
- Per-file downloads via `requestdl`, preserve folder structure (no zip/extract).
- On Sonarr delete: remove from TorBox **and** local.
- **TorBox slot model (corrected 2026-06-16).** A torrent occupies one of the
  account's concurrent slots **only while `active`** — downloading/caching, or
  *seeding*. A **cached** torrent that isn't seeding is **inactive** and holds **no
  slot** (`mylist` says so directly: `active: bool`, `download_state: "cached"`). We
  run with **seeding disabled** (TorBox account setting; `createtorrent` sends no
  `seed`, so the account default applies), so a torrent goes inactive the instant it
  finishes caching and **frees its slot automatically**. It does *not* "seed forever"
  or pin a slot until deleted — earlier docs claimed that and were wrong.
- Still delete from TorBox on COMPLETE, and still run the **reaper** (reconcile loop +
  startup) over leftover COMPLETE/ERROR torrents — but both are now **hygiene, not
  slot recovery**: once the bytes are on local disk we don't need TorBox's copy, so we
  drop it to keep `mylist` tidy. A non-seeding cached torrent already holds no slot, so
  nothing "starves the queue" if a delete is missed. (Local files + DB row stay,
  reported `pausedUP`, so Sonarr can still import; the later Sonarr delete just clears
  local state.)
- **Slot accounting.** True occupancy = torrents **actively caching** (`active==true`),
  not the count still carrying a `torbox_id`. ⚠️ **Known gap:** the submitter currently
  gates on `CountOnTorBox` (`store/writes.go` — rows with a non-null
  `torbox_id`/`torbox_queued_id`), which **over-counts**: it includes cached `LOCAL_*`
  and un-reaped COMPLETE rows that aren't active on TorBox. Safe (it can't over-submit
  past 3) but it **throttles the queue** — e.g. while pulling two large cached torrents
  to disk it behaves as if 2 slots are busy though TorBox sees 0 active. The correct
  gate counts only active/caching torrents; left conservative for now.
- **Don't use TorBox's own queue.** Our QUEUED state is the single queue. Because we
  gate on real occupancy, `createtorrent` shouldn't be queued by TorBox; if it is
  (an orphan torrent we don't track holds a slot), cancel the queued entry via
  `controlqueued` and retry from our own queue. The old "adopt the queued_id, re-match
  by infohash on activation" path was removed — it was a fragile second namespace.
- Headless: YAML/env config, SQLite state, logs (no Web UI in v1).
- Fail on *stall* (not on slowness), with **progress-tiered patience** (2026-07-12):
  a fetch stalled at 0% fails after `failure.stall_timeout` (default 20m — cheap to
  abandon, lets Sonarr re-grab); one with real progress gets
  `failure.progress_stall_timeout` (default 2h — thin swarms recover seeds on a scale
  of hours, and failing blacklists what is often the only viable release); cached
  releases get `failure.cached_stall_timeout` (default 30m). While stalled the engine
  nudges TorBox with `controltorrent reannounce` every `failure.reannounce_interval`
  (default 5m). Cache check on add stays informational. Optional absolute
  `failure.timeout` cap, disabled by default. See Gotchas.
- No auth on the qBittorrent API (LAN trust).
- Pure-Go stack (no cgo) for easy Unraid Docker builds: `modernc.org/sqlite`,
  `anacrolix/torrent/metainfo` for infohash, stdlib `net/http`+`slog`.

## Hard constraints
- TorBox Essential plan: **3 concurrent slots**, **200GB max per download**, no Usenet,
  `createtorrent` **60/hour**, general 300/min, `requestdl` links valid ~3h.
- The reported `hash` must be the exact v1 infohash Sonarr computed, or Sonarr loses the
  download. Whole-series packs may exceed 200GB → expect TorBox reject → report `error`
  (correct behavior; Sonarr falls back to a season pack).

## Gotchas
- COMPLETE must be reported as `pausedUP` (an "UP" state). Never `pausedDL` — Sonarr treats
  `*DL` as not-done.
- A fetching torrent is failed (→ ERROR, so Sonarr blacklists the release) only when it
  *stalls*: no bytes moving and progress not climbing for its tier's grace —
  `failure.stall_timeout` (default 20m) at 0% progress, `failure.progress_stall_timeout`
  (default 2h) once it has real progress, `failure.cached_stall_timeout` (default 30m) for
  cached. It is never failed just for being slow — a download still moving bytes keeps
  resetting the stall clock (tracked via `torrents.progress_at`). Stalled fetches get a
  TorBox `reannounce` nudge every `failure.reannounce_interval` (default 5m; throttled via
  the engine's in-memory `lastReannounce`). `failure.timeout` is an optional absolute cap
  from active_since, disabled by default. All these clocks run only while TORBOX_ACTIVE,
  not while waiting in our own queue.
- TorBox may **queue** a `createtorrent` (returns a queued id, not a torrent id) when the
  account's active slots are full. We no longer adopt that queue: we gate submissions on real
  slot occupancy so it shouldn't happen, and if it does we `controlqueued`-delete the entry and
  leave the torrent in our own QUEUED to retry. (With seeding disabled a torrent goes inactive
  once cached and stops counting toward the cap on its own; reaping is just cleanup.) On 429 the submitter pauses for a cooldown
  (hourly quota). A torrent absent from `mylist` is failed only after a grace window **and** a
  direct id lookup confirms it's gone.
- Download into an incomplete dir, then atomic move, so Sonarr never imports partial files.
- Some TorBox JSON field names in API_REFERENCE.md come from secondary sources — **verify
  against the live API in M1** before depending on them.

## Environment
- Go 1.26 available locally. Git repo, `main` branch. CWD: `/home/guibibi/projects/tordownloader`.
- TorBox API key required to run/test the TorBox client (M1+). Keep it out of git.
