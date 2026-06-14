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
- Headless: YAML/env config, SQLite state, logs (no Web UI in v1).
- Fail-fast: cache check on add; ERROR if not present within **20 min** of becoming active.
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
- The fail-fast 20-min clock runs only while TORBOX_ACTIVE, not while waiting in our own queue.
- Download into an incomplete dir, then atomic move, so Sonarr never imports partial files.
- Some TorBox JSON field names in API_REFERENCE.md come from secondary sources — **verify
  against the live API in M1** before depending on them.

## Environment
- Go 1.26 available locally. Git repo, `main` branch. CWD: `/home/guibibi/projects/tordownloader`.
- TorBox API key required to run/test the TorBox client (M1+). Keep it out of git.
