<p align="center">
  <img src="docs/img/mascot.svg" width="260" alt="A happy cardboard box licking its lips while video files drop into it">
</p>

<h1 align="center">tordownloader</h1>

<p align="center">
  <b>Your TorBox account, wearing a qBittorrent costume.</b><br>
  Sonarr and Radarr send it torrents. You get real files on your disk. No mounts, no rclone, no fuss.
</p>

<p align="center">
  <a href="https://github.com/Guibibi/tordownloader/tags"><img src="https://img.shields.io/github/v/tag/Guibibi/tordownloader?sort=semver&label=version" alt="Version"></a>
  <a href="https://github.com/Guibibi/tordownloader/actions/workflows/docker-publish.yml"><img src="https://github.com/Guibibi/tordownloader/actions/workflows/docker-publish.yml/badge.svg" alt="Build"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/Guibibi/tordownloader" alt="Go"></a>
  <a href="https://github.com/Guibibi/tordownloader/pkgs/container/tordownloader"><img src="https://img.shields.io/badge/ghcr.io-guibibi%2Ftordownloader-2496ED?logo=docker&logoColor=white" alt="Container"></a>
</p>

---

## Why you'd want this

You pay for [TorBox](https://torbox.app). It already has half the internet cached and pulls
everything else at silly speeds. But Sonarr and Radarr don't know how to talk to it, and the usual
fix is a FUSE mount that streams files over the network and falls over when you look at it funny.

tordownloader is the boring fix. It answers on the qBittorrent API, so you add it in Sonarr exactly
like you'd add qBittorrent. Behind the scenes it hands the torrent to TorBox, waits for it, then
downloads the finished files into `/downloads` like any other client would. Sonarr imports them,
hardlinks work, and your library is made of real files.

Grab an episode, and a few minutes later it's sitting in Plex. That's the whole pitch.

<p align="center">
  <img src="docs/img/dashboard.png" alt="The tordownloader dashboard showing torrents caching on TorBox, one season pack downloading to disk with its per-file list, and one queued" width="900">
  <br><sub>The built-in dashboard at <code>http://your-server:6500/</code> (demo data)</sub>
</p>

## What you get

- **It's just qBittorrent, as far as Sonarr knows.** No plugins, no forks, no scripts. Works with
  Sonarr, Radarr, and remote apps like **nzb360** too.
- **Real files on disk.** Folder structure is kept as-is, so season packs land as season packs.
- **No half-imported episodes.** Files download into a staging folder and only move into place once
  they're complete.
- **Bad releases get swapped out automatically.** If a torrent stalls, or crawls along too slowly to
  ever finish, tordownloader gives up on it and tells Sonarr/Radarr to blocklist it and grab
  something else. You wake up to the episode, not to a stuck queue.
- **Patient with the good ones.** Something stuck at 0% gets dropped after about 20 minutes. Something
  that's actually making progress gets hours, because thin swarms often recover.
- **Plays nice with your plan.** It knows how many TorBox slots you have (3 on Essential), keeps
  its own queue for the rest, and stays under the API rate limits.
- **Cleans up after itself.** Remove a download in Sonarr and it's gone from TorBox and from disk.
- **Won't fill your drive.** A download only starts if it actually fits, with a 10 GB cushion left
  over. Otherwise it waits its turn.
- **Hard to wedge.** Dead CDN connections get retried with a fresh link, and the download picks up
  from what's already on disk.
- **Light.** One small Go binary in a Docker image, SQLite for state, no database server, no cgo.
  Prometheus metrics at `/metrics` if you're into graphs.

## How it fits together

```
Prowlarr ──> Sonarr/Radarr ──(qBittorrent API)──> tordownloader ──(TorBox API)──> TorBox
                  ^                                      │
                  └────────── imports from ──────────────┘
                          shared /downloads folder
```

1. Sonarr grabs a release and sends it to "qBittorrent" (that's us).
2. We pass it to TorBox and keep an eye on it.
3. Once TorBox has it, we download the files to `/downloads/<category>/<name>/`.
4. Sonarr sees it finished and imports it into your library.
5. When Sonarr removes the download, we clean up TorBox and the local copy.

## Get it running in 5 minutes

You'll need:

- A **TorBox** account and its API key (you'll find it in your TorBox account settings).
- **Sonarr and/or Radarr** already running.
- A **`/downloads` folder that Sonarr/Radarr can see at the same path**. This is the one thing
  people trip on, more on it below.

### Docker Compose

Grab [`docker-compose.yml`](docker-compose.yml), point the `/downloads` volume at the same folder
Sonarr uses, then:

```bash
TORBOX_API_KEY=your-key docker compose up -d
```

### Unraid

**Docker → Add Container:**

| Field | Value |
|---|---|
| Repository | `ghcr.io/guibibi/tordownloader:latest` |
| Port | `6500` → `6500` |
| Path `/data` | `/mnt/user/appdata/tordownloader/` |
| Path `/downloads` | `/mnt/user/downloads/` |
| Variable `TORBOX_API_KEY` | your TorBox API key |
| Variable `PUID` / `PGID` | `99` / `100` |

Hit **Apply** and you're done.

<details>
<summary><b>Plain <code>docker run</code></b></summary>

```bash
docker run -d \
  --name tordownloader \
  -e TORBOX_API_KEY=your-api-key \
  -e PUID=99 -e PGID=100 \
  -v /path/to/data:/data \
  -v /mnt/user/downloads:/downloads \
  -p 6500:6500 \
  --restart unless-stopped \
  ghcr.io/guibibi/tordownloader:latest
```
</details>

<details>
<summary><b>Build it yourself</b></summary>

Needs Go 1.26+.

```bash
go build ./cmd/tordownloader
# or the Docker image:
docker build --build-arg VERSION=v0.14.0 -t tordownloader:v0.14.0 .
```
</details>

Images are multi-arch (amd64 and arm64). Use `:latest`, or pin a release like `:v0.14.0`.

## Hook up Sonarr and Radarr

**Settings → Download Clients → + → qBittorrent**

| Field | Value |
|---|---|
| Host | your server's IP, e.g. `192.168.1.50` |
| Port | `6500` |
| Username / Password | anything you like, it doesn't check |
| Category | `tv-sonarr` for Sonarr, `radarr` for Radarr |

Hit **Test**, then **Save**. Same deal in **nzb360**: add a qBittorrent server with that host and
port and your downloads show up there too.

> [!IMPORTANT]
> **Map `/downloads` to the same path everywhere.** tordownloader, Sonarr and Radarr all need to see
> the files at the exact same path, or imports fail (or crawl along as copies instead of instant
> hardlinks). LinuxServer images use `/downloads` already. On binhex or hotio images, set
> `TD_DOWNLOAD_ROOT` to whatever path they use.

### Let it replace failed downloads (do this one)

Here's a fun Sonarr fact: it ignores qBittorrent's error state completely. So when a release dies,
Sonarr just leaves it sitting in the queue forever. Give tordownloader your Sonarr/Radarr API keys
and it'll tell them directly: blocklist this release, go find another one.

```bash
TD_SONARR_URL=http://192.168.1.50:8989
TD_SONARR_API_KEY=your-sonarr-api-key
TD_RADARR_URL=http://192.168.1.50:7878
TD_RADARR_API_KEY=your-radarr-api-key
```

Both the URL and the key are needed for each one. If only half is set, it's skipped and you'll see a
warning in the log.

## Settings

The defaults are fine for most people. Everything can be set with environment variables:

| Variable | Default | What it does |
|---|---|---|
| `TORBOX_API_KEY` | *(required)* | Your TorBox API key |
| `PUID` / `PGID` | `99` / `100` | Who owns the downloaded files |
| `TD_LISTEN_ADDR` | `0.0.0.0:6500` | Where the API and dashboard listen |
| `TD_DOWNLOAD_ROOT` | `/downloads` | Where files are saved |
| `TD_DB_PATH` | `/data/tordownloader.db` | The SQLite state file |
| `TD_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `TD_LOG_FORMAT` | `text` | `text` or `json` |
| `TD_SONARR_URL` / `TD_SONARR_API_KEY` | *(unset)* | Sonarr, for replacing failed downloads |
| `TD_RADARR_URL` / `TD_RADARR_API_KEY` | *(unset)* | Same, for Radarr |

Want to tweak the slot count, how long to wait on slow or stuck torrents, the speed floor, the
free-space cushion, or the poll interval? Those live in a config file. Copy
[`config.example.yaml`](config.example.yaml) (every option has a comment), mount it, and start with
`--config /config.yaml`.

## Good to know

- **Big packs.** TorBox's Essential plan caps a single download at 200 GB. A full-series pack bigger
  than that gets rejected, and Sonarr falls back to individual seasons. That's expected.
- **On startup** it checks your TorBox account and logs a heads-up if your key is wrong, your
  subscription is about to expire, or your slot setting doesn't match your plan.
- **Health and metrics.** `/healthz` returns `ok` while things are healthy (the Docker healthcheck
  uses it), and `/metrics` is Prometheus-ready.
- **Failed torrents tidy themselves up** after 7 days once they've been dealt with.
- **No auth.** It's meant to live on your LAN next to Sonarr. Don't put it on the open internet.

## Under the hood

If you want the details: [Architecture](docs/ARCHITECTURE.md) covers the moving parts and the state
machine, and the [API reference](docs/API_REFERENCE.md) lists every qBittorrent endpoint we fake and
every TorBox endpoint we call.

Inspired by [rdt-client](https://github.com/rogerfar/rdt-client) and
[decypharr](https://github.com/sirrobot01/decypharr). This one is TorBox-only, written in Go,
headless, and downloads to disk instead of mounting.
