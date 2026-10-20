# AnimeWorld Indexer

A [Torznab](https://torznab.github.io/spec-1.3-draft/) indexer for [AnimeWorld](https://www.animeworld.ac), so Prowlarr, Sonarr and Radarr can search it. One Go binary, one container, SQLite cache.

Status: keyword, TVDB-numbered TV search and movie search, a Cloudflare fallback and grab-to-import through an emulated qBittorrent client work end to end. The download queue survives restarts. Planned next: RSS.

## What it does

- `GET /api?t=caps` and `GET /api?t=search&q=<title>` (also `tvsearch`) return one RSS item per episode, category `5070` (TV/Anime). `t=movie` returns films, category `2000` (see [Movies](#movies)).
- `t=tvsearch` supports `q`, `season`, `ep` and `tvdbid` (see [TVDB numbering](#tvdb-numbering)). `t=search` accepts a trailing `S02E03` in `q` as a season/episode filter; any other `q` (including titles ending in a number) goes to the site as is.
- Titles look like `Sousou no Frieren S02E03 [SUB ITA]`. Dubbed entries (separate series on the site) get `[ITA]`.
- Every item has a size and date from a `HEAD` on the episode's direct MP4. Lookups are cached in SQLite and bounded; if a search runs out of time budget, the remaining sizes are estimated from the same series while the real ones finish in the background.
- An empty `q` returns the site's latest updates, which is what Prowlarr's indexer Test uses.
- `GET /download?ep=<token>&n=<title>` redirects to a magnet (`magnet:?xt=urn:btih:<hash>&dn=<title>&aw=<token>`). The hash is derived from the episode token; `aw` carries the token itself. Sonarr and Radarr pass that magnet to their download client.
- `/api/v2/...` emulates the **qBittorrent Web API** (the subset Sonarr and Radarr use). Adding the magnet downloads the episode's MP4 over HTTP to `<DOWNLOAD_DIR>/<category>/<title>.mp4` and reports progress and completion like a torrent, so they import the file from the shared volume. See [Download queue](#download-queue) for parallelism, retries and restarts.
- `GET /health` returns JSON status, including a `solver` object: `configured`, `url`, `cookies_valid` (valid solved Cloudflare cookies are cached) and `expires_at`. Logs are JSON on stdout.

### Cloudflare fallback

AnimeWorld answers plain requests today, but sits behind Cloudflare. When it returns a challenge (403/503 with `cf-mitigated: challenge` or the "Just a moment" page), the indexer asks the FlareSolverr-compatible solver at `FLARESOLVERR_URL` (Byparr, default `http://byparr:8191`) to open the site once, then reuses the returned cookies and user agent for plain requests. They are cached in the SQLite database for 12 hours (surviving restarts) and renewed as soon as a request is challenged again. Only one solve runs at a time; concurrent challenged requests wait for it. A solve can take 40-130 s, so the solver timeout is 120 s and a search that needs one is slow once.

If the solver is unreachable or fails, the error is logged and the Torznab search returns an empty result (not an error); further requests within 30 s fail fast instead of waiting on the solver again.

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
| Categories | `5070` (TV/Anime), plus `2000` (Movies) for Radarr |

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

Run the container as the user that owns the media (`user: "1000:1000"` in the example), so the imported files have the right owner. The image creates `/app/data` and `/downloads` owned by 1000:1000; a bind-mounted folder keeps the owner it has on the host, so make it writable by that user. A download that receives no data for 60 s, or answers with a web page instead of a video, fails and shows as an error in the client.

If you search through Prowlarr, add the indexer to Sonarr/Radarr via Prowlarr as usual (Prowlarr passes the magnet on); no extra setup is needed. In Sonarr, leave "Remove Completed" on: once a file is imported the finished entry is removed together with the downloaded copy.

### Download queue

Jobs and categories are kept in the SQLite database (`DATA_DIR/cache.db`), so a restart or crash loses nothing.

- **Parallelism.** `MAX_DOWNLOADS` (default `2`) episodes download at once; a season grab's other episodes show as `queuedDL` and start as slots free up.
- **Restart.** Unfinished downloads (running or queued) start again by themselves and continue the `.part` file with a `Range` request; if the server does not answer `206` the file is downloaded again from the start. Finished jobs whose file is still in the downloads folder stay as `pausedUP` until the client deletes them; a finished job whose file is gone is dropped. `.part` files that no job owns are deleted at startup. Categories are restored too, so `save_path` is right before the first new add.
- **Retries.** A failed attempt (5xx, timeout, stall, a Cloudflare page instead of a video, a short body) is retried up to 3 times in all, 2 s then 4 s apart, each time asking the site for a fresh file URL and resuming the partial file. A 404/410 is not retried. Only after that is the job `error`, which Sonarr/Radarr show as a warning until you remove it; adding the same episode again retries it.
- **Stopped adds.** `paused=true` / `stopped=true` on `torrents/add` adds the job as `pausedDL` without downloading. `torrents/pause` and `stop` pause a job (the partial file stays), `resume` and `start` continue it.
- **Delete.** `torrents/delete` stops the job and removes its row and partial file; with `deleteFiles=true` it also removes the finished file.
- **One episode, one category.** An episode is tracked once, whichever client grabbed it. If Sonarr and Radarr (or two categories) ask for the same episode while it is still in the queue, the second add is answered `Fails.` and logged; it works again once the first job is deleted. Set `Remove Completed` as described above so finished entries do not linger.

### TVDB numbering

AnimeWorld lists every season as its own series, numbered from episode 1 ("Frieren 2", episode 3), while Sonarr thinks in TVDB seasons. The service bridges them with the [Fribb/anime-lists](https://github.com/Fribb/anime-lists) `anime-list-full.json`, which maps MyAnimeList and AniList ids to a TVDB id, TVDB season and episode offset (a second cour continuing a season has an offset: AnimeWorld episode 1 of "Re:Zero 2 Part 2" is TVDB `S02E14`). It also carries the TMDB and IMDb ids.

- Every AnimeWorld series page links its MAL and AniList ids; those are looked up in the list. The list is downloaded in the background (a search never waits for it), kept in the SQLite cache and downloaded again every `MAPPING_REFRESH` (24h). Until the first download finishes, and whenever a refresh fails, searches use the previous copy or, with none, the title heuristic below.
- `t=tvsearch&tvdbid=…` returns only series mapped to that TVDB id, filtered by `season`/`ep`. If Prowlarr sends no `q`, the show is found by the requested season's MAL title (looked up from AniList and cached; a failed lookup is remembered for an hour), then by shortened forms of it and of the AnimePlanet slug in the list. Series without a mapping are only accepted from your own `q`, never from these fallbacks.
- A series missing from the list falls back to a heuristic: a trailing season number in the title or slug (`Frieren 2`, `Season 2`, `2nd Season`, `Overlord III`, `-2-ita`) gives that season, otherwise season 1. When the title is given, such unmapped series are returned for a TVDB search too; their season marker is stripped from the title (`Frieren 2` is released as `Frieren S02E03`) so Sonarr matches the show.
- Releases always use `SxxEyy` titles, which Sonarr parses for standard and Anime-type series. Only an episode with no integer number (such as `12.5`) keeps the site's own number: `Title - 12.5 [SUB ITA]`. Items also carry `tvdbid`, `season` and `episode` Torznab attributes when known.

### Movies

AnimeWorld lists a film as a single-episode series whose "Categoria" is `Movie` (search results carry a `Movie` badge). `t=movie` supports `q`, `imdbid` (with or without `tt`) and `tmdbid`, which Prowlarr and Radarr send because the caps declare them.

- With an id, the film is found through the same Fribb list as TV (it carries IMDb and TMDB movie ids; a TMDB id is only matched against films, never against TV shows with the same number): by `q` when sent, then by the film's MAL title (from AniList) and its AnimePlanet slug. Only series mapped to that id are returned, sub and dub as separate releases. If the list does not know the id at all, unmapped films found through `q` are returned instead, without ids.
- Without an id, `q` is a keyword search limited to the results marked `Movie`.
- Titles look like `Your Name. 2016 [SUB ITA]` (`[ITA]` for the dub); the year is the site's release date, left out when the page has none. Items are category `2000` and carry `imdb` (bare number), `imdbid` (`tt…`) and `tmdbid` attributes when the list ties the film to them. A film is its first episode; they download through the same `/download` link as TV episodes.
- Like the TV releases, titles carry no quality, so Radarr sees quality "Unknown": allow it in the quality profile used for AnimeWorld.

### Configuration

All environment variables are listed, with defaults, in [`.env.example`](.env.example).

## Develop

```bash
go vet ./...
go test ./...
API_KEY=dev DATA_DIR=./data go run ./cmd/animeworldtv-indexer
curl 'localhost:9698/api?t=search&q=frieren&apikey=dev'
curl 'localhost:9698/api?t=tvsearch&tvdbid=424536&season=2&ep=3&apikey=dev'   # Frieren S02E03
curl 'localhost:9698/api?t=movie&imdbid=tt5311514&apikey=dev'                # Your Name. (sub and dub)
```

Requires Go 1.27+. The first search of a title makes a few hundred requests to the site (series pages, then file info and a `HEAD` per episode); repeat searches come from the cache.

## Credits

Started as a fork of [TuxDave's AnimeDownloader](https://github.com/TuxDave/AnimeDownloader), a Java Swing client for AnimeWorld. None of its code remains, but it showed the way in. Licensed under the [MIT License](LICENSE).

This project is not affiliated with AnimeWorld.
