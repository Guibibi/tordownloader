# tordownloader

A self-hosted bridge that lets **Sonarr/Radarr** use **TorBox** (a debrid service) as a
download client — without any rclone/WebDAV/fuse mount. It emulates the qBittorrent WebUI
API, forwards releases to TorBox, waits for them to finish on TorBox's servers, then
**downloads the real files to your local disk** in the right category folders for Sonarr/Radarr
to import.

Think of it as [rdt-client](https://github.com/rogerfar/rdt-client) /
[decypharr](https://github.com/sirrobot01/decypharr), but **TorBox-only, Go, headless, and
download-to-disk only** (no mount-based streaming).

## How it fits

```
Prowlarr ──> Sonarr/Radarr ──(qBittorrent API)──> tordownloader ──(TorBox API)──> TorBox
                  ^                                      │
                  └──────── imports from ────────────────┘
                           shared /downloads volume
```

1. Sonarr/Radarr grab a release and send the magnet/torrent to tordownloader (thinking it's qBittorrent).
2. tordownloader forwards it to TorBox and tracks progress.
3. When TorBox has the files, tordownloader downloads them to `/<root>/<category>/<name>/...`.
4. Sonarr/Radarr see the completed download and import (hardlink/copy) into the library.
5. On removal, tordownloader deletes the torrent from TorBox and the local source copy.

## Status

Pre-implementation. Requirements and design are documented; no code written yet.

## Docs

- [docs/PRD.md](docs/PRD.md) — product requirements & scope
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — components, state machine, data model
- [docs/API_REFERENCE.md](docs/API_REFERENCE.md) — qBittorrent endpoints emulated + TorBox endpoints used + state mapping
- [docs/ROADMAP.md](docs/ROADMAP.md) — implementation milestones

## Target deployment

Single Docker container on an Unraid host, alongside Sonarr/Radarr/Prowlarr, sharing the
`/downloads` volume. Headless: configured via YAML/env, state in SQLite, observed via logs.
