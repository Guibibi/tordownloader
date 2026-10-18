# Changelog

All notable changes to tordownloader. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/) (pre-1.0: minor versions may change behavior).

Docker images for each version are published as `ghcr.io/guibibi/tordownloader:<version>`.

## [Unreleased]

### Added
- MIT license.
- Unraid template (`unraid/tordownloader.xml`) with the port, `/downloads` and `/data` paths, TorBox
  key, Sonarr/Radarr failure push-back variables, and advanced PUID/PGID/log settings. It runs
  with `--config /data/config.yaml`, so a `config.yaml` placed in appdata is used when present.
- PNG logo for the template icon; README install steps for the template.

### Changed
- README rewritten in a plainer tone, with a logo and a dashboard screenshot.

## [0.14.0] - 2026-10-18

### Added
- Prometheus metrics at `/metrics`: torrents by state, TorBox slots used/max, TorBox and local
  download speed, free disk space, and counters for failures by reason, local retries, bytes
  downloaded, completions, TorBox API responses and Sonarr/Radarr push-backs.
- Free-disk-space check: a download only starts or resumes when free space covers what it still
  has to write, what running downloads still need, and a reserve (`download.min_free_space`).
  Otherwise it waits, without failing, until space frees up.
- TorBox account check at startup: a rejected API key, an expired or expiring subscription, an
  account cooldown, the Free plan, and a slot setting that doesn't match the plan are logged.
- `sync/maindata` and detail-view endpoints, so remote-control apps such as nzb360 and the
  qBittorrent WebUI show the torrent list.
- CI runs gofmt, `go vet` and `go test -race` on every push and pull request.

### Changed
- **The new free-space reserve defaults to 10 GB.** On a share with less free space than that,
  downloads now wait instead of starting. Set `download.min_free_space: 0` to only require that
  each download fits, or `-1` to turn the check off.
- Torrents are always submitted with seeding disabled, instead of following the TorBox account
  setting, so a finished torrent never keeps holding a slot.
- Failed local downloads (CDN outage, link throttling, full disk) are retried with backoff from 1
  minute up to 30 minutes, and failed only after 8 attempts, instead of failing right away. A full
  or read-only disk never uses up attempts.
- `createtorrent` errors caused by TorBox or the account (outage, expired key, rate or plan
  limits) leave the torrent queued instead of failing it. Before, one outage could fail and
  blocklist every queued release.
- Adding a release that previously failed queues it again instead of leaving it failed.

### Fixed
- A torrent removed on TorBox's side is now detected (TorBox answers with 404), instead of staying
  active forever and holding a slot.
- Sonarr/Radarr "Remove Completed" works: torrents now report `ratio_limit: 0`.
- A fetch that took a while to find peers could be failed by the slow-speed check right after it
  started; waiting time before the first byte no longer counts against its average.
- A long `Retry-After` from TorBox (for example the hourly `createtorrent` quota) no longer blocks
  a worker for hours.
- When TorBox queued a submission, it was re-submitted every 2 seconds and used up the hourly
  quota; submissions now pause for 10 minutes.
- `content_path` comes from the files' top-level folder, not the display name, which could point
  Sonarr at a folder that doesn't exist.
- File paths that are absolute or escape with `..` are rejected instead of being written outside
  the download folder.
- On shutdown the engine and in-flight downloads finish before the database is closed.

## [0.13.0] - 2026-09-01

### Added
- Sustained-speed check: once a TorBox fetch has started, it must average at least
  `failure.min_speed` (default 50 KB/s) over each `failure.slow_window` (default 15 minutes), or it
  is failed and pushed back to Sonarr/Radarr. A trickling torrent used to hold a slot forever.
  `min_speed: 0` turns this off.
- `download.idle_timeout` (default 2 minutes): a CDN transfer that stays open but stops sending is
  aborted and resumed with a fresh link.

## [0.12.0] - 2026-07-21

### Added
- Several torrents download to disk in parallel (`download.parallel_torrents`, default 3).
- Failed torrents that have been handled are deleted after `failure.error_retention` (default
  7 days).
- `/healthz` endpoint backed by the database, used by the Docker healthcheck.
- A warning when only one of a Sonarr/Radarr URL and API key pair is set.

### Fixed
- Download progress stays accurate across retries, and flaky transfers are retried.

## [0.11.0] - 2026-07-21

### Added
- Failed downloads are pushed back to Sonarr/Radarr through their API (`arr:` config or
  `TD_SONARR_*` / `TD_RADARR_*`): the release is blocklisted and a replacement is searched.
  Sonarr/Radarr don't do this on their own for a qBittorrent client in an error state.

## [0.10.0] - 2026-07-12

### Added
- Stall timeouts depend on progress: 20 minutes at 0%, 2 hours once a fetch has made progress,
  30 minutes for a cached release still at 0%. Stalled fetches are sent a TorBox reannounce every
  5 minutes.

### Fixed
- Only torrents TorBox is actively fetching count toward the slot limit.

## [0.9.0] - 2026-06-15

### Changed
- Submissions are limited by actual TorBox slot use, and completed torrents still on TorBox are
  cleaned up.

## [0.8.0] - 2026-06-15

### Changed
- A torrent is deleted from TorBox once its files are on local disk.

## [0.7.0] - 2026-06-15

### Changed
- Docker images are published only for version tags; pushes to `main` and pull requests only
  build.

## [0.6.0] - 2026-06-15

### Added
- TorBox API calls are rate-limited to stay under TorBox's 300 requests per minute
  (`torbox.max_requests_per_min`).

### Fixed
- A rate-limited download link request is retried instead of failing the torrent.
- `SQLITE_BUSY` errors under concurrent access.

## [0.5.0] - 2026-06-15

### Added
- Separate stall grace period for releases TorBox already has cached.

### Fixed
- Downloads queued on TorBox's side are cleaned up correctly.

## [0.4.0] - 2026-06-15

### Added
- Dashboard: per-file breakdown, stall countdown, seeders/peers, TorBox ETA, cache hits, and
  active torrents sorted first.

### Fixed
- Transient CDN errors are retried instead of failing the torrent.

## [0.3.0] - 2026-06-14

### Fixed
- Submissions that TorBox queues are handled, and a torrent missing from TorBox is confirmed
  before it is failed.

## [0.2.0] - 2026-06-14

### Changed
- Torrents fail when they stall, not when they are slow.
- Failed torrents are deleted from TorBox.

## [0.1.0] - 2026-06-14

First release.

- qBittorrent WebUI API v2 emulation for Sonarr/Radarr.
- Submits to TorBox within the plan's slot limit and downloads finished files to disk per file,
  keeping the folder structure, through a staging folder and an atomic move.
- Removing a download in Sonarr/Radarr deletes it from TorBox and from disk.
- Recovers state on restart and resumes partial downloads.
- Status dashboard, Docker image with PUID/PGID for Unraid, published to GHCR.

[Unreleased]: https://github.com/Guibibi/tordownloader/compare/v0.14.0...HEAD
[0.14.0]: https://github.com/Guibibi/tordownloader/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/Guibibi/tordownloader/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/Guibibi/tordownloader/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/Guibibi/tordownloader/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/Guibibi/tordownloader/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/Guibibi/tordownloader/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/Guibibi/tordownloader/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/Guibibi/tordownloader/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/Guibibi/tordownloader/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/Guibibi/tordownloader/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/Guibibi/tordownloader/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/Guibibi/tordownloader/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Guibibi/tordownloader/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Guibibi/tordownloader/releases/tag/v0.1.0
