# tordownloader

**Use [TorBox](https://torbox.app) as a download client for Sonarr & Radarr — no mount, real files on disk.**

[![Version](https://img.shields.io/github/v/tag/Guibibi/tordownloader?sort=semver&label=version)](https://github.com/Guibibi/tordownloader/tags)
[![Build](https://github.com/Guibibi/tordownloader/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/Guibibi/tordownloader/actions/workflows/docker-publish.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/Guibibi/tordownloader)](go.mod)
[![Container](https://img.shields.io/badge/ghcr.io-guibibi%2Ftordownloader-2496ED?logo=docker&logoColor=white)](https://github.com/Guibibi/tordownloader/pkgs/container/tordownloader)

tordownloader is a small, headless Go service that pretends to be qBittorrent so **Sonarr/Radarr**
hand it releases. It forwards them to **TorBox** (a debrid service), waits for TorBox to fetch them,
then **downloads the finished files to your local disk** in the right category folders — ready for
Sonarr/Radarr to import. No rclone, WebDAV, or fuse mount.

Think [rdt-client](https://github.com/rogerfar/rdt-client) /
[decypharr](https://github.com/sirrobot01/decypharr), but **TorBox-only, Go, headless, and
download-to-disk** (no mount-based streaming).

## Features

- **Drop-in qBittorrent client** — Sonarr/Radarr talk to it exactly like qBittorrent (WebUI API v2); no plugins or patches.
- **Real downloads to disk** — pulls finished files from TorBox to `/<root>/<category>/<name>/…`, preserving folder structure. No mount, no streaming.
- **Safe imports** — downloads into a `.incomplete` staging dir, then atomic-moves into place, so Sonarr/Radarr never import a half-written file.
- **Plan-aware** — respects TorBox's concurrent-slot limit (3 on Essential), queues the rest, and paces API calls to stay under TorBox's rate limit.
- **Fails on stalls, not slowness** — a dead/unseeded release is errored so Sonarr blacklists it and grabs another, but a slow-but-moving download is left to finish.
- **Clean teardown** — when Sonarr/Radarr remove a download, it's deleted from TorBox and local disk too.
- **Status dashboard** — a built-in web page shows live per-torrent and per-file progress.
- **Headless & light** — YAML/env config, SQLite state, structured logs; a single static binary, no cgo.

## How it works

```
Prowlarr ──> Sonarr/Radarr ──(qBittorrent API)──> tordownloader ──(TorBox API)──> TorBox
                  ^                                      │
                  └──────────── imports from ───────────┘
                            shared /downloads volume
```

1. Sonarr/Radarr grab a release and send the magnet/torrent to tordownloader (thinking it's qBittorrent).
2. tordownloader forwards it to TorBox and tracks progress.
3. When TorBox has the files, tordownloader downloads them to `/<root>/<category>/<name>/…`.
4. Sonarr/Radarr see the completed download and import (hardlink/copy) into your library.
5. On removal, tordownloader deletes the torrent from TorBox and the local source copy.

> [!TIP]
> Once it's running, open **`http://<host>:6500/`** in a browser for a live status dashboard — per-torrent state, TorBox-vs-disk progress, and an expandable per-file breakdown.

## Prerequisites

- A **TorBox** account and API key (the Essential plan — 3 slots — is the tested target).
- **Sonarr/Radarr** (and usually Prowlarr) already running.
- A **`/downloads` volume shared** with Sonarr/Radarr, mapped to the *same path* in every container so imports can hardlink.
- **Docker** (or Unraid's Docker UI).

> [!NOTE]
> TorBox's Essential plan caps each download at 200 GB. A whole-series pack over that is rejected by TorBox and reported as failed — which is expected: Sonarr then falls back to grabbing individual season packs.

## Quick start (Docker Compose)

Grab [`docker-compose.yml`](docker-compose.yml) from the repo, point the `/downloads` volume at the
same path Sonarr/Radarr use, then:

```bash
TORBOX_API_KEY=your-key docker compose up -d
```

That's it — now [connect Sonarr/Radarr](#connect-sonarrradarr).

Pre-built multi-arch images (amd64/arm64) are on GitHub Container Registry:

```
ghcr.io/guibibi/tordownloader:latest      # newest release
ghcr.io/guibibi/tordownloader:v0.6.0      # or pin a specific version
```

<details>
<summary><b>Unraid (Docker UI)</b></summary>

**Docker → Add Container:**

| Field | Value |
|---|---|
| Repository | `ghcr.io/guibibi/tordownloader:latest` |
| Port | `6500` → `6500` |
| Volume `/data` | `/mnt/user/appdata/tordownloader/` |
| Volume `/downloads` | `/mnt/user/downloads/` |
| Variable `TORBOX_API_KEY` | `your-torbox-api-key` |
| Variable `PUID` | `99` |
| Variable `PGID` | `100` |

Click **Apply**, then [connect Sonarr/Radarr](#connect-sonarrradarr).
</details>

<details>
<summary><b>docker run</b></summary>

```bash
docker run -d \
  --name tordownloader \
  -e TORBOX_API_KEY=your-api-key \
  -e PUID=99 \
  -e PGID=100 \
  -v /path/to/data:/data \
  -v /mnt/user/downloads:/downloads \
  -p 6500:6500 \
  --restart unless-stopped \
  ghcr.io/guibibi/tordownloader:latest
```
</details>

<details>
<summary><b>Build from source</b></summary>

Requires Go 1.26+.

```bash
go build ./cmd/tordownloader              # local binary
# …or build the Docker image at a version:
docker build --build-arg VERSION=v0.6.0 -t tordownloader:v0.6.0 .
```
</details>

## Connect Sonarr/Radarr

In Sonarr/Radarr: **Settings → Download Clients → + → qBittorrent**

| Field | Value |
|---|---|
| Host | Your host/Unraid IP (e.g. `192.168.1.50`) |
| Port | `6500` |
| Username / Password | anything (auth is stubbed for LAN use) |
| Category | `tv-sonarr` (Sonarr) or `radarr` (Radarr) |

> [!IMPORTANT]
> The `/downloads` volume must be mapped to the **same path** in tordownloader, Sonarr, and Radarr —
> otherwise imports can't hardlink and will fail or fall back to slow copies. LinuxServer.io images
> use `/downloads` (matching our default); for binhex/hotio, set `TD_DOWNLOAD_ROOT` to match theirs.

## Configuration

Defaults suit most setups. Override with environment variables, or mount a `config.yaml` for the
full set of knobs (see [`config.example.yaml`](config.example.yaml), which documents every option):

| Env var | Default | Description |
|---|---|---|
| `TORBOX_API_KEY` | *(required)* | TorBox API key |
| `PUID` | `99` | User ID for downloaded-file ownership |
| `PGID` | `100` | Group ID for downloaded-file ownership |
| `TD_LISTEN_ADDR` | `0.0.0.0:6500` | qBittorrent API listen address |
| `TD_DOWNLOAD_ROOT` | `/downloads` | Downloads save path |
| `TD_DB_PATH` | `/data/tordownloader.db` | SQLite database path |
| `TD_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `TD_LOG_FORMAT` | `text` | `text` \| `json` |

> [!TIP]
> Finer controls — TorBox slot count, stall timeouts, API rate cap, poll interval — live in
> `config.yaml` only. Mount one and pass `--config /config.yaml`; start from
> [`config.example.yaml`](config.example.yaml).

## Documentation

| Doc | What's inside |
|---|---|
| [Architecture](docs/ARCHITECTURE.md) | Components, state machine, data model |
| [API Reference](docs/API_REFERENCE.md) | qBittorrent endpoints emulated, TorBox endpoints used, state mapping |

## Status

Feature-complete and running in production on Unraid. Multi-arch images are published to GHCR via
GitHub Actions on every tagged release.
