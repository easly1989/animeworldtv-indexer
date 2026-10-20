# AnimeWorld Indexer

A [Torznab](https://torznab.github.io/spec-1.3-draft/) indexer for [AnimeWorld](https://www.animeworld.ac), so Prowlarr, Sonarr and Radarr can search it. One Go binary, one container, SQLite cache.

Status: keyword search and grab-to-import work end to end. Planned next: TVDB mapping, a Cloudflare fallback, a persistent download queue, RSS and movies.

## What it does

- `GET /api?t=caps` and `GET /api?t=search&q=<title>` (also `tvsearch`, `movie`) return one RSS item per episode, category `5070` (TV/Anime).
- Titles look like `Sousou no Frieren - 01 [SUB ITA]`. Dubbed entries (separate series on the site) get `[ITA]`.
- Every item has a size and date from a `HEAD` on the episode's direct MP4. Lookups are cached in SQLite and bounded; if a search runs out of time budget, the remaining sizes are estimated from the same series while the real ones finish in the background.
- An empty `q` returns the site's latest updates, which is what Prowlarr's indexer Test uses.
- `GET /download?ep=<token>&n=<title>` redirects to a magnet (`magnet:?xt=urn:btih:<hash>&dn=<title>&aw=<token>`). The hash is derived from the episode token; `aw` carries the token itself. Sonarr and Radarr pass that magnet to their download client.
- `/api/v2/...` emulates the **qBittorrent Web API** (the subset Sonarr and Radarr use). Adding the magnet downloads the episode's MP4 over HTTP to `<DOWNLOAD_DIR>/<category>/<title>.mp4` and reports progress and completion like a torrent, so they import the file from the shared volume. The queue is in memory for now: a restart forgets running downloads.
- `GET /health` returns JSON status. Logs are JSON on stdout.

## Run

```bash
cp .env.example .env     # set API_KEY
docker compose -f docker-compose.example.yml up -d
```

or `docker run -p 9698:9698 -e API_KEY=... -v ./data:/app/data ghcr.io/easly1989/animeworldtv-indexer:latest`.

Default port is **9698** (Prowlarr uses 9696, mircrewrr 9697).

### Prowlarr

Add an indexer of type **Generic Torznab**:

| Field | Value |
|---|---|
| URL | `http://animeworld-indexer:9698` |
| API Path | `/api` |
| API Key | your `API_KEY` |
| Categories | `5070` (TV/Anime) |

### Sonarr / Radarr

Add a download client of type **qBittorrent**:

| Field | Value |
|---|---|
| Host | `animeworld-indexer` (the container name) |
| Port | `9698` |
| Use SSL | off |
| Username | anything, for example `animeworld` |
| Password | your `API_KEY` |
| Category | for example `sonarr` (files land in `/downloads/sonarr/`) |

The **Test** button should pass. Sonarr/Radarr must see the downloads folder at the same path as this container, or you need a Remote Path Mapping (host `animeworld-indexer`). In the compose example the volume is `/volume1/media/downloads:/downloads` on both sides.

Run the container as the user that owns the media (`user: "1000:1000"` in the example), so the imported files have the right owner. `/app/data` and the downloads folder must be writable by that user.

If you search through Prowlarr, add the indexer to Sonarr/Radarr via Prowlarr as usual (Prowlarr passes the magnet on); no extra setup is needed. In Sonarr, leave "Remove Completed" on: once a file is imported the finished entry is removed together with the downloaded copy.

### Configuration

All environment variables are listed, with defaults, in [`.env.example`](.env.example).

## Develop

```bash
go vet ./...
go test ./...
API_KEY=dev DATA_DIR=./data go run ./cmd/animeworldtv-indexer
curl 'localhost:9698/api?t=search&q=frieren&apikey=dev'
```

Requires Go 1.27+. The first search of a title makes a few hundred requests to the site (series pages, then file info and a `HEAD` per episode); repeat searches come from the cache.

## Credits

Started as a fork of [TuxDave's AnimeDownloader](https://github.com/TuxDave/AnimeDownloader), a Java Swing client for AnimeWorld. None of its code remains, but it showed the way in. Licensed under the [MIT License](LICENSE).

This project is not affiliated with AnimeWorld.
