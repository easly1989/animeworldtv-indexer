# AnimeWorld Indexer

A [Torznab](https://torznab.github.io/spec-1.3-draft/) indexer for [AnimeWorld](https://www.animeworld.ac), so Prowlarr, Sonarr and Radarr can search it. One Go binary, one container, SQLite cache.

Status: keyword search works end to end. Planned next: an emulated qBittorrent API that downloads the episodes, TVDB mapping, a Cloudflare fallback, RSS and movies.

## What it does

- `GET /api?t=caps` and `GET /api?t=search&q=<title>` (also `tvsearch`, `movie`) return one RSS item per episode, category `5070` (TV/Anime).
- Titles look like `Sousou no Frieren - 01 [SUB ITA]`. Dubbed entries (separate series on the site) get `[ITA]`.
- Every item has a size and date from a `HEAD` on the episode's direct MP4. Lookups are cached in SQLite and bounded; if a search runs out of time budget, the remaining sizes are estimated from the same series while the real ones finish in the background.
- An empty `q` returns the site's latest updates, which is what Prowlarr's indexer Test uses.
- `GET /download?ep=<token>` currently redirects to the MP4. It will become the hand-off to the download client.
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
