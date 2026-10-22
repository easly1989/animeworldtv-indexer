# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go service that exposes AnimeWorld (`https://www.animeworld.ac`; the `.tv`/`.so` domains redirect there) as a **Torznab indexer** for Prowlarr/Sonarr/Radarr. It began as a fork of TuxDave's Java Swing AnimeDownloader, which was removed. Besides searches it serves RSS and movies, maps series to TVDB seasons and episodes via MAL/AniList ids, gets past Cloudflare through Byparr, emulates a qBittorrent client that downloads the MP4s, and gives mircrewrr's admin panel a status/log API and settings it can change. The sibling project `mircrewrr` (Python/Flask) runs in the same compose stack: Prowlarr 9696, mircrewrr 9697, this service 9698 (container `animeworld-indexer`).

## Build, test, run

```bash
go vet ./...
go test ./...                 # parsers run against fixtures in internal/animeworld/testdata
go run ./cmd/animeworldtv-indexer
API_KEY=dev DATA_DIR=./data go run ./cmd/animeworldtv-indexer   # then: curl 'localhost:9698/api?t=caps&apikey=dev'
docker build -t animeworldtv-indexer .
```

Pure Go, no CGO (SQLite is `modernc.org/sqlite`). Config is environment variables only; see `.env.example` and `internal/config`.

## Architecture

`cmd/animeworldtv-indexer` wires everything; the packages under `internal/` depend downward only:

- `torznab` — HTTP handler: `/api` (`t=caps|search|tvsearch|movie`, apikey check, Torznab XML written by hand to keep the `torznab:` prefix) and `/download?ep=<token>`. Talks to a small `Backend` interface.
- `indexer` — the service: search → series pages → per-episode releases. Owns caching in SQLite, a shared request semaphore, and the file-size time budget (unmeasured sizes are estimated while lookups finish in the background). Depends on `Site` and `Cache` interfaces so tests use fakes.
- `animeworld` — scraping. `parse.go` is pure (HTML/JSON in, structs out); `client.go` does the HTTP (one retry on 5xx/transport errors); `polite.go` holds the origin rate limiter (a transport wrapper, so redirect hops count) and the ban handling: a 403/429 without Cloudflare challenge markers is `ErrOriginBlocked` (an `ErrBlocked`), the solver is not called, and origin requests fail fast for 5 min doubling to 1 h. Never test against the live site: it bans IPs.
- `mapping` — AnimeWorld series (MAL/AniList ids) to TVDB/TMDB ids, seasons and episode offsets, from the Fribb anime-lists; seasons guessed from the title when it has nothing.
- `solver` — Cloudflare challenges through a FlareSolverr-compatible service (Byparr); cookies and user agent are reused and cached on disk.
- `qbit` — the subset of qBittorrent's Web API v2 that Sonarr/Radarr use. A "torrent" is an episode: `torrents/add` takes the feed's magnet, `downloader` fetches the MP4.
- `downloader` — the download queue behind `qbit`; with a store, jobs survive a restart and resume from the bytes on disk, at most `MaxDownloads` at once.
- `status` — `GET /api/status` and `GET /api/logs` (SSE) for mircrewrr's panel, read server-side, plus `GET/PUT /api/settings` and `POST /api/restart` (exits; Docker starts it again).
- `settings` — values changed from the panel, kept in `DATA_DIR/settings.json`; they override the environment from the next start, so a change needs a restart. An unusable file is quarantined with a date, not used.
- `store` — SQLite key/value cache with per-read max age. Later features add their own tables.
- `config` — env parsing; `main` lays the panel's saved settings over it at start.

Things that span files and bite:

- Selectors are tied to AnimeWorld's current markup (`.film-list .item a.name`, `h1#anime-title`, `a[data-episode-num][data-id]`, `a#mal-button`/`a#anilist-button`). Parsers return an error when the anchor element is missing (a broken selector must not look like "no results"). When one breaks, fetch the live page, save it under `testdata`, fix the test first.
- Dubbed versions are separate site entries whose slug ends in `-ita`; the release title carries `[ITA]` for those and `[SUB ITA]` otherwise. Seasons are separate entries too.
- An empty `q` without `tvdbid` is the RSS path (`Service.latest`): the `/updated` listing parsed by `ParseUpdated` (episode number and token come from the listing; a Prowlarr indexer Test needs it non-empty). `/search?keyword=` with no keyword redirects away, so `Client.Search` needs a keyword.
- A series path is `/play/<slug>.<id>`; the site 302s it to the first episode page. The direct file comes from `/api/episode/info?id=<token>` (`grabber`), and a HEAD on it gives `Content-Length` and `Last-Modified` (used as pubDate).
- `/download?ep=<token>&n=<title>` 302s to a magnet that carries the episode token, so the arr hands it to the emulated qBittorrent (`qbit`), which downloads the MP4. It is not authenticated, like mircrewrr's.

## Agent skills

### Issue tracker

GitHub Issues on `easly1989/animeworldtv-indexer`, via `gh`. See `docs/agents/issue-tracker.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the root. See `docs/agents/domain.md`.

### Token tools

CBM: call `index_repository` once, then `search_graph` / `trace_path` before reading files.
