// Package config reads the service settings from environment variables, in
// the same style as the sibling mircrewrr project.
package config

import (
	"fmt"
	"log/slog"
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

	// FlareSolverrURL is the Byparr/FlareSolverr endpoint; unused until the
	// Cloudflare fallback lands.
	FlareSolverrURL string

	MaxSeries    int           // series expanded per search
	Concurrency  int           // simultaneous requests to AnimeWorld
	SearchBudget time.Duration // how long a search waits for file sizes

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
	num := func(key string, def, min int) int {
		v := str(key, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err != nil || n < min {
			errs = append(errs, fmt.Sprintf("%s=%q must be an integer >= %d", key, v, min))
			return def
		}
		return n
	}
	duration := func(key, def string) time.Duration {
		v := str(key, def)
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%q must be a positive duration like %s", key, v, def))
		}
		return d
	}
	d := duration("SEARCH_BUDGET", "40s")
	refresh := duration("MAPPING_REFRESH", "24h")

	c := Config{
		APIKey:          getenv("API_KEY"),
		Host:            str("PROXY_HOST", "0.0.0.0"),
		Port:            num("PROXY_PORT", 9698, 1),
		DataDir:         str("DATA_DIR", "/app/data"),
		BaseURL:         strings.TrimRight(str("ANIMEWORLD_URL", "https://www.animeworld.ac"), "/"),
		LogLevel:        strings.ToUpper(str("LOG_LEVEL", "INFO")),
		FlareSolverrURL: str("FLARESOLVERR_URL", "http://byparr:8191"),
		MaxSeries:       num("MAX_SERIES", 8, 1),
		Concurrency:     num("CONCURRENCY", 4, 1),
		SearchBudget:    d,
		MappingURL:      str("MAPPING_URL", "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-full.json"),
		MappingRefresh:  refresh,
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// SlogLevel maps LOG_LEVEL (DEBUG, INFO, WARNING, ERROR) to slog.
func (c Config) SlogLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.Replace(c.LogLevel, "WARNING", "WARN", 1))); err != nil {
		return slog.LevelInfo
	}
	return l
}
