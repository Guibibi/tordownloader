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
- **Slot accounting (gap fixed).** True occupancy = torrents **actively caching**, and
  that is what the submitter gates on: `CountActiveSlots` (`store/writes.go`) counts only
  `TORBOX_ACTIVE` rows. Cached `LOCAL_*`, COMPLETE, and ERROR rows hold no slot and are
  excluded even while they still carry a `torbox_id`. (The old `CountOnTorBox` gate that
  over-counted those rows — a "known gap" in earlier revisions of this file — is gone.)
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
- **Failed-download push-back via the *arr API (2026-07-21).** Sonarr *never* runs
  failed-download handling off qBittorrent states — its `error` case is commented
  "warning so failed download handling isn't triggered" — so reporting `state=error`
  alone leaves the item stuck in Sonarr's queue with no blocklist/re-grab (the
  original "so Sonarr blacklists" assumption was wrong). Fix: `internal/arr` +
  config `arr:` (or `TD_SONARR_URL`/`TD_SONARR_API_KEY`, `TD_RADARR_URL`/
  `TD_RADARR_API_KEY`). The reconcile loop's arrPass takes ERROR rows with
  `arr_notified=0`, finds the *arr queue record by `downloadId` == infohash, and
  `DELETE /api/v3/queue/{id}?removeFromClient=true&blocklist=true&skipRedownload=false`
  → the *arr blocklists, deletes the torrent from us (row + partial files), and
  searches for a replacement. Retries: unreachable *arr → every 1m; clean
  not-found → retried for 10m (a fast fail can beat the *arr queue refresh),
  then marked notified and left as ERROR.
- **ERROR retention (2026-07-21).** Settled ERROR rows (notified — or *arr push-back not
  configured) that sit unchanged for `failure.error_retention` (default 168h; negative
  disables) are pruned via `DeleteTorrent` (TorBox leftovers + partial files + row), so
  terminal failures don't accumulate forever. Rows still awaiting *arr notification are
  never pruned.
- `/healthz` (DB-backed liveness; runs `SELECT 1`) is what the Dockerfile/compose
  healthchecks hit — `app/version` is static and stays green even with a wedged store.
- **Parallel local downloads (2026-07-21).** Up to `download.parallel_torrents`
  (default 3) torrents are pulled to disk concurrently, each in its own goroutine
  bounded by `download.parallel_files` — one huge grab no longer blocks cached
  episodes behind it. Concurrency/dup-start gating and per-torrent delete
  cancellation live in `Engine.activeDownloads` (infohash → cancel).
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
- A fetching torrent is failed (→ ERROR; the arr push-back then makes Sonarr blocklist
  the release and grab another — the reported `error` state alone does *nothing* in
  Sonarr) only when it
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
