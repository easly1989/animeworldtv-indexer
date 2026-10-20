// Command animeworldtv-indexer is a Torznab indexer for AnimeWorld.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/config"
	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
	"github.com/easly1989/animeworldtv-indexer/internal/indexer"
	"github.com/easly1989/animeworldtv-indexer/internal/mapping"
	"github.com/easly1989/animeworldtv-indexer/internal/qbit"
	"github.com/easly1989/animeworldtv-indexer/internal/solver"
	"github.com/easly1989/animeworldtv-indexer/internal/store"
	"github.com/easly1989/animeworldtv-indexer/internal/torznab"
)

var version = "dev" // set by -ldflags in the Docker build

// How long solved Cloudflare cookies are trusted (cf_clearance lasts longer; same as mircrewrr).
const solverTTL = 12 * time.Hour

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()})))
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		slog.Warn("API_KEY is empty: the Torznab and qBittorrent APIs are open to anyone who can reach them")
	}

	db, err := store.Open(filepath.Join(cfg.DataDir, "cache.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()

	site := animeworld.New(cfg.BaseURL, nil)
	var sol *solver.Solver
	if cfg.FlareSolverrURL != "" {
		sol = solver.New(cfg.FlareSolverrURL, cfg.BaseURL, solverTTL, db)
		site.UseSolver(sol)
	}
	maps := mapping.NewSource(cfg.MappingURL, db, cfg.MappingRefresh)
	maps.Index(context.Background()) // starts the first download in the background
	svc := indexer.New(site, db, indexer.Options{
		MaxSeries: cfg.MaxSeries, Concurrency: cfg.Concurrency, Budget: cfg.SearchBudget, Mapping: maps,
		Titles: mapping.NewTitles(mapping.AniListURL, db),
	})

	dl := downloader.New(svc, cfg.DownloadDir, nil)
	defer dl.Close()

	mux := http.NewServeMux()
	mux.Handle("/api/v2/", qbit.New(qbit.Options{APIKey: cfg.APIKey}, dl))
	mux.Handle("/", torznab.New(torznab.Options{APIKey: cfg.APIKey, SiteURL: cfg.BaseURL}, svc))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": version, "site": cfg.BaseURL, "solver": sol.Status()})
	})

	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("listening", "addr", srv.Addr, "site", cfg.BaseURL, "version", version)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
