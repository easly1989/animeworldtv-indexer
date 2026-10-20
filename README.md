# AnimeWorld Indexer

A [Torznab](https://torznab.github.io/spec-1.3-draft/) indexer for [AnimeWorld](https://www.animeworld.ac), so Prowlarr, Sonarr and Radarr can search it. One Go binary, one container, SQLite cache.

Status: keyword and TVDB-numbered TV search work end to end. Planned next: an emulated qBittorrent API that downloads the episodes, a Cloudflare fallback, RSS and movies.

## What it does

- `GET /api?t=caps` and `GET /api?t=search&q=<title>` (also `tvsearch`, `movie`) return one RSS item per episode, category `5070` (TV/Anime).
- `t=tvsearch` supports `q`, `season`, `ep` and `tvdbid` (see [TVDB numbering](#tvdb-numbering)). `t=search` accepts a trailing `S02E03` in `q` as a season/episode filter; a trailing bare number is dropped from the site search and does not filter.
- Titles look like `Sousou no Frieren S02E03 [SUB ITA]`. Dubbed entries (separate series on the site) get `[ITA]`.
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

### TVDB numbering

AnimeWorld lists every season as its own series, numbered from episode 1 ("Frieren 2", episode 3), while Sonarr thinks in TVDB seasons. The service bridges them with the [Fribb/anime-lists](https://github.com/Fribb/anime-lists) `anime-list-full.json`, which maps MyAnimeList and AniList ids to a TVDB id, TVDB season and episode offset (a second cour continuing a season has an offset: AnimeWorld episode 1 of "Re:Zero 2 Part 2" is TVDB `S02E14`). It also carries the TMDB and IMDb ids.

- Every AnimeWorld series page links its MAL and AniList ids; those are looked up in the list. The list is downloaded once, kept in the SQLite cache and downloaded again every `MAPPING_REFRESH` (24h). If a refresh fails the previous copy keeps working.
- `t=tvsearch&tvdbid=…` returns only series mapped to that TVDB id, filtered by `season`/`ep`. If Prowlarr sends no `q`, the show is found by its MAL title (looked up once from AniList and cached) and, failing that, by the AnimePlanet slug in the list.
- A series missing from the list falls back to a heuristic: a trailing season number in the title or slug (`Frieren 2`, `Season 2`, `2nd Season`, `Overlord III`, `-2-ita`) gives that season, otherwise season 1. When the title is given, such unmapped series are returned for a TVDB search too; their season marker is stripped from the title (`Frieren 2` is released as `Frieren S02E03`) so Sonarr matches the show.
- Releases always use `SxxEyy` titles, which Sonarr parses for standard and Anime-type series. Only an episode with no integer number (such as `12.5`) keeps the site's own number: `Title - 12.5 [SUB ITA]`. Items also carry `tvdbid`, `season` and `episode` Torznab attributes when known.

### Configuration

All environment variables are listed, with defaults, in [`.env.example`](.env.example).

## Develop

```bash
go vet ./...
go test ./...
API_KEY=dev DATA_DIR=./data go run ./cmd/animeworldtv-indexer
curl 'localhost:9698/api?t=search&q=frieren&apikey=dev'
curl 'localhost:9698/api?t=tvsearch&tvdbid=424536&season=2&ep=3&apikey=dev'   # Frieren S02E03
```

Requires Go 1.27+. The first search of a title makes a few hundred requests to the site (series pages, then file info and a `HEAD` per episode); repeat searches come from the cache.

## Credits

Started as a fork of [TuxDave's AnimeDownloader](https://github.com/TuxDave/AnimeDownloader), a Java Swing client for AnimeWorld. None of its code remains, but it showed the way in. Licensed under the [MIT License](LICENSE).

This project is not affiliated with AnimeWorld.
