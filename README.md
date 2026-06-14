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

## Running with Docker

Pre-built images are published to **GitHub Container Registry** on every push to `main`.

```
ghcr.io/guibibi/tordownloader:latest
```

### Unraid quick start

In the Unraid Docker UI (**Docker → Add Container**):

| Field | Value |
|---|---|
| Repository | `ghcr.io/guibibi/tordownloader:latest` |
| Port | `6500` → `6500` |
| Volume `/data` | `/mnt/user/appdata/tordownloader/` |
| Volume `/downloads` | `/mnt/user/downloads/` |
| Variable `TORBOX_API_KEY` | `your-torbox-api-key` |
| Variable `PUID` | `99` |
| Variable `PGID` | `100` |

Click **Apply**, then add `http://<unraid-ip>:6500` as a qBittorrent client in Sonarr/Radarr.

### docker compose

```bash
# Create a compose file (see docker-compose.yml in the repo)
TORBOX_API_KEY=your-key docker compose up -d
```

### Run manually

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

### Sonarr/Radarr client setup

In Sonarr/Radarr: **Settings → Download Clients → + → qBittorrent**

| Field | Value |
|---|---|
| Host | Your Unraid IP (e.g. `192.168.1.50`) |
| Port | `6500` |
| Username / Password | anything (auth is stubbed) |
| Category | `tv-sonarr` (Sonarr) or `radarr` (Radarr) |

**Important**: the `/downloads` volume must be mapped identically across tordownloader, Sonarr, and Radarr. Linuxserver.io containers default to `/downloads` which matches our default. If you use binhex or hotio, set `TD_DOWNLOAD_ROOT` to match.

### Configuration

All settings have defaults suitable for most setups (see [config.example.yaml](config.example.yaml)).
Override them via environment variables or mount a `config.yaml`:

| Env var | Default | Description |
|---|---|---|
| `TORBOX_API_KEY` | *(required)* | TorBox API key |
| `PUID` | `99` | User ID for downloaded file ownership |
| `PGID` | `100` | Group ID for downloaded file ownership |
| `TD_LISTEN_ADDR` | `0.0.0.0:6500` | qBittorrent API listen address |
| `TD_DOWNLOAD_ROOT` | `/downloads` | Downloads save path |
| `TD_DB_PATH` | `/data/tordownloader.db` | SQLite database path |
| `TD_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `TD_LOG_FORMAT` | `text` | `text` \| `json` |

Build the image at a specific version tag:

```bash
docker build --build-arg VERSION=v1.0.0 -t tordownloader:v1.0.0 .
```
