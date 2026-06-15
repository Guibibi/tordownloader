# CLAUDE.md — orientation for future sessions

## What this is
**tordownloader**: a Go service that emulates the **qBittorrent WebUI API v2** so
**Sonarr/Radarr** use **TorBox** (debrid) as a download client, then downloads finished files
to local disk. No mount — real downloads. Like rdt-client/decypharr but TorBox-only, Go,
headless, download-to-disk. Runs as one Docker container on the user's **Unraid** host
(Sonarr/Radarr/Prowlarr already there; `/downloads` shared and mapped identically).

## Read first
- `docs/PRD.md` — scope & requirements
- `docs/ARCHITECTURE.md` — components, state machine, data model, tech choices
- `docs/API_REFERENCE.md` — qBit endpoints emulated + TorBox endpoints used + **state mapping**
- `docs/ROADMAP.md` — milestones (M0–M8); start at the lowest incomplete one

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
  account's active slots are full — common when bulk-adding past the **60/hour** createtorrent
  cap. Such a torrent isn't in `mylist` until it activates (under a *new* id). The reconciler
  marks it `torbox_state=queued`, matches it back by infohash when it activates, and never
  treats queued/absent as "vanished". Absence is only failed after a grace window **and** a
  direct id lookup confirms it. On 429 the submitter pauses for a cooldown (hourly quota).
- Download into an incomplete dir, then atomic move, so Sonarr never imports partial files.
- Some TorBox JSON field names in API_REFERENCE.md come from secondary sources — **verify
  against the live API in M1** before depending on them.

## Environment
- Go 1.26 available locally. Git repo, `main` branch. CWD: `/home/guibibi/projects/tordownloader`.
- TorBox API key required to run/test the TorBox client (M1+). Keep it out of git.
