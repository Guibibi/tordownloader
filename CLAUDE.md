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
- Free the TorBox slot as soon as the local download completes: on COMPLETE we
  delete the torrent from TorBox (slots are scarce — 3 — and TorBox seeds forever
  otherwise, starving the queue). Local files + DB row stay (reported `pausedUP`)
  so Sonarr can still import; the later Sonarr delete then just clears local state.
  Don't rely on Sonarr's "Remove Completed" to free slots — it's deferred/optional.
  The delete-on-complete is **not** the only safety net: a **reaper** in the reconcile
  loop (and on startup) deletes any COMPLETE/ERROR torrent still on TorBox and clears
  its refs, so a delete missed by an old build, a crash, or a transient error
  self-heals instead of pinning a slot forever.
- **Slot accounting = real occupancy, not TORBOX_ACTIVE count.** A torrent holds a
  TorBox slot from `createtorrent` until it's deleted — through caching, local
  download, and COMPLETE-not-yet-reaped. The submitter gates on the count of rows
  still holding a `torbox_id`/`torbox_queued_id`, and a successful TorBox delete
  clears those refs. Undercounting (gating on TORBOX_ACTIVE only) was the bug that
  over-submitted and let completed-but-undeleted torrents starve the queue.
- **Don't use TorBox's own queue.** Our QUEUED state is the single queue. Because we
  gate on real occupancy, `createtorrent` shouldn't be queued by TorBox; if it is
  (an orphan torrent we don't track holds a slot), cancel the queued entry via
  `controlqueued` and retry from our own queue. The old "adopt the queued_id, re-match
  by infohash on activation" path was removed — it was a fragile second namespace.
- Headless: YAML/env config, SQLite state, logs (no Web UI in v1).
- Fail on *stall* (not on slowness): cache check on add (informational); ERROR only when a
  fetching torrent makes no progress for `failure.stall_timeout` (default 10m). Optional
  absolute `failure.timeout` cap, disabled by default. See Gotchas.
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
  *stalls*: no bytes moving and progress not climbing for `failure.stall_timeout` (default
  10m). It is never failed just for being slow — a download still moving bytes keeps resetting
  the stall clock (tracked via `torrents.progress_at`). `failure.timeout` is an optional
  absolute cap from active_since, disabled by default. Both clocks run only while
  TORBOX_ACTIVE, not while waiting in our own queue.
- TorBox may **queue** a `createtorrent` (returns a queued id, not a torrent id) when the
  account's active slots are full. We no longer adopt that queue: we gate submissions on real
  slot occupancy so it shouldn't happen, and if it does we `controlqueued`-delete the entry and
  leave the torrent in our own QUEUED to retry. (Reaping completed torrents keeps occupancy
  honest, so a healthy account stays below the cap.) On 429 the submitter pauses for a cooldown
  (hourly quota). A torrent absent from `mylist` is failed only after a grace window **and** a
  direct id lookup confirms it's gone.
- Download into an incomplete dir, then atomic move, so Sonarr never imports partial files.
- Some TorBox JSON field names in API_REFERENCE.md come from secondary sources — **verify
  against the live API in M1** before depending on them.

## Environment
- Go 1.26 available locally. Git repo, `main` branch. CWD: `/home/guibibi/projects/tordownloader`.
- TorBox API key required to run/test the TorBox client (M1+). Keep it out of git.
