// Package config reads the service settings from environment variables, in
// the same style as the sibling mircrewrr project.
package config

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIKey   string // empty disables the apikey check
	Host     string
	Port     int
	DataDir  string
	BaseURL  string // AnimeWorld; the .tv/.so domains redirect to .ac
	LogLevel string

	// FlareSolverrURL is the Byparr/FlareSolverr endpoint used when Cloudflare
	// challenges a request.
	FlareSolverrURL string

	// DownloadDir is where the emulated qBittorrent client saves episodes, in
	// <DownloadDir>/<category>/. Sonarr/Radarr must see the same path.
	DownloadDir string

	// MaxDownloads is how many episodes the qBittorrent client downloads at
	// once; the rest wait as queuedDL.
	MaxDownloads int

	MaxSeries    int           // series expanded per search
	Concurrency  int           // simultaneous requests to AnimeWorld
	SearchBudget time.Duration // how long a search waits for file sizes
	SizeLookups  int           // uncached file sizes resolved per response; the rest are estimated

	OriginRPS   float64 // requests per second to AnimeWorld itself (not its file servers)
	OriginBurst int     // how many may go out back to back

	MappingURL     string        // Fribb anime-list-full.json
	MappingRefresh time.Duration // how often the mapping list is downloaded again
}

// Load builds a Config from getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var errs []string
	// The ranges keep a typo (or a panel edit) from unthrottling the site, which bans IPs,
	// or from stalling the service.
	num := func(key string, def, min, max int) int {
		v := str(key, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			errs = append(errs, fmt.Sprintf("%s=%q must be an integer from %d to %d", key, v, min, max))
			return def
		}
		return n
	}
	duration := func(key, def string, min, max time.Duration) time.Duration {
		v := str(key, def)
		d, err := time.ParseDuration(v)
		if err != nil || d < min || d > max {
			errs = append(errs, fmt.Sprintf("%s=%q must be a duration from %s to %s", key, v, min, max))
		}
		return d
	}
	rpsText := str("ORIGIN_RPS", "1")
	rps, err := strconv.ParseFloat(rpsText, 64)
	if err != nil || !(rps >= 0.1 && rps <= 10) || math.IsNaN(rps) {
		errs = append(errs, fmt.Sprintf("ORIGIN_RPS=%q must be a number from 0.1 to 10", rpsText))
		rps = 1
	}
	level := strings.ToUpper(str("LOG_LEVEL", "INFO"))
	switch level {
	case "DEBUG", "INFO", "WARNING", "WARN", "ERROR":
	default:
		errs = append(errs, fmt.Sprintf("LOG_LEVEL=%q must be DEBUG, INFO, WARNING or ERROR", level))
	}
	d := duration("SEARCH_BUDGET", "40s", 5*time.Second, 5*time.Minute)
	refresh := duration("MAPPING_REFRESH", "24h", time.Minute, 30*24*time.Hour)

	c := Config{
		APIKey:          getenv("API_KEY"),
		Host:            str("PROXY_HOST", "0.0.0.0"),
		Port:            num("PROXY_PORT", 9698, 1, 65535),
		DataDir:         str("DATA_DIR", "/app/data"),
		BaseURL:         strings.TrimRight(str("ANIMEWORLD_URL", "https://www.animeworld.ac"), "/"),
		LogLevel:        level,
		FlareSolverrURL: str("FLARESOLVERR_URL", "http://byparr:8191"),
		DownloadDir:     str("DOWNLOAD_DIR", "/downloads"),
		MaxDownloads:    num("MAX_DOWNLOADS", 2, 1, 10),
		MaxSeries:       num("MAX_SERIES", 8, 1, 50),
		Concurrency:     num("CONCURRENCY", 4, 1, 16),
		SearchBudget:    d,
		SizeLookups:     num("SIZE_LOOKUPS", 10, 0, 100),
		OriginRPS:       rps,
		OriginBurst:     num("ORIGIN_BURST", 3, 1, 20),
		MappingURL:      str("MAPPING_URL", "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-full.json"),
		MappingRefresh:  refresh,
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// Editable returns the settings the admin panel may change, keyed by their
// environment variable, in the form Load accepts.
func (c Config) Editable() map[string]string {
	return map[string]string{
		"MAX_DOWNLOADS": strconv.Itoa(c.MaxDownloads),
		"ORIGIN_RPS":    strconv.FormatFloat(c.OriginRPS, 'g', -1, 64),
		"ORIGIN_BURST":  strconv.Itoa(c.OriginBurst),
		"SIZE_LOOKUPS":  strconv.Itoa(c.SizeLookups),
		"MAX_SERIES":    strconv.Itoa(c.MaxSeries),
		"CONCURRENCY":   strconv.Itoa(c.Concurrency),
		"SEARCH_BUDGET": c.SearchBudget.String(),
		"LOG_LEVEL":     c.LogLevel,
	}
}

// SlogLevel maps LOG_LEVEL (DEBUG, INFO, WARNING, ERROR) to slog.
func (c Config) SlogLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.Replace(c.LogLevel, "WARNING", "WARN", 1))); err != nil {
		return slog.LevelInfo
	}
	return l
}
