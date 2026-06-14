# tordownloader — Implementation Roadmap

Milestones are ordered so each one is independently testable. "DoD" = definition of done.

## M0 — Project scaffold
- `go mod init`, package layout per ARCHITECTURE.md §2.
- Config loader (YAML + env) with defaults; `config.example.yaml`.
- slog logging; graceful shutdown.
- SQLite store with migrations for `torrents`, `files`, `categories`.
- **DoD**: binary starts, loads config, opens/migrates the DB, logs, exits cleanly.

## M1 — TorBox client + API verification
- Hand-rolled client (evaluate `torbox-sdk-go`) for: `createtorrent`, `mylist`,
  `checkcached`, `requestdl`, `controltorrent`. Bearer auth + rate-limit backoff.
- **Verify against the live API** (with the user's key) the exact JSON shapes that the docs
  here describe from secondary sources: `createtorrent` response (torrent_id), `mylist`
  torrent/file fields (`download_present`, `files[].id/name/short_name`), `requestdl` response
  (URL vs redirect), and the `>200GB` error code. Update API_REFERENCE.md with confirmed truth.
- **DoD**: can submit a magnet, poll its status, list files, and fetch a download URL via tests
  against the real API (gated behind an env var / not in CI).

## M2 — qBittorrent emulation (read/identity)
- HTTP server + routes for `auth/login`, `app/version`, `app/webapiVersion`,
  `app/preferences`, `torrents/info`, `torrents/categories`, `transfer/info`.
- Render the torrent object (API_REFERENCE §2) from the store.
- **DoD**: Sonarr/Radarr can add tordownloader as a qBittorrent client and the connection test
  passes; an empty torrent list shows up.

## M3 — Add flow + infohash
- `torrentmeta`: extract v1 infohash from magnet and `.torrent` (bencode + SHA1), normalise.
- `torrents/add` (magnet, http, multipart); category create/set; persist + enqueue.
- Submission gate honoring `max_active_slots` (3); `createtorrent` call; rate-limit backoff.
- **DoD**: grabbing a release in Sonarr creates a TorBox torrent and a tracked row with the
  correct hash; queue respects the slot limit.

## M4 — Reconciler + state machine
- Poll `mylist`; map TorBox state + local progress → internal state → qBittorrent state.
- Blended progress; fail-fast 20-min timer (only while TORBOX_ACTIVE); cached check on add.
- ERROR on TorBox rejection / timeout / vanished torrent.
- **DoD**: Sonarr shows live progress; a dead release flips to `error` within ~20 min and
  Sonarr blacklists + re-grabs.

## M5 — Downloader
- On `download_present`: enumerate files, `requestdl` per file, download with bounded
  concurrency into incomplete dir, atomic move, mark COMPLETE.
- Resume partial files (HTTP Range); re-request expired links; verify sizes.
- Report `pausedUP` + valid `content_path` only when all files are on disk.
- **DoD**: a real grab lands in `/downloads/<category>/<name>/...` and Sonarr imports it.

## M6 — Delete / cleanup
- `torrents/delete`: cancel in-flight, `controltorrent` delete on TorBox, remove local files,
  drop DB rows.
- **DoD**: after Sonarr imports and removes, the torrent is gone from TorBox and the local
  source copy is deleted.

## M7 — Resilience
- Restart reconciliation: rebuild in-memory state from SQLite + `mylist`; resume downloads.
- Idempotent add; handle duplicate grabs; bound retries with backoff.
- **DoD**: killing/restarting the container mid-download resumes without data loss or
  re-grabbing completed items.

## M8 — Packaging for Unraid
- Multi-stage Dockerfile (pure-Go, no cgo) → small final image.
- PUID/PGID entrypoint for correct file ownership; `docker-compose.yml` example;
  Unraid Community Apps template (later).
- **DoD**: runs on Unraid sharing `/downloads` with Sonarr/Radarr; files owned correctly.

## Backlog / later
- Optional Web UI (keep engine/store API clean to allow it).
- Usenet via SABnzbd emulation (only if the user upgrades to a plan with Usenet).
- `checkcached` pre-filtering to skip obviously-uncached releases faster.
- Metrics endpoint.

## Open items to confirm during build
- Exact TorBox JSON field names / response shapes (M1 verification).
- Whether to gate slots ourselves vs rely on TorBox `as_queued` (lean: gate ourselves).
- qBit state to use for QUEUED (`stalledDL` vs `metaDL`) — pick what keeps Sonarr most patient.
