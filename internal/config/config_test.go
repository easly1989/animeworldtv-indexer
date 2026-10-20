package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9698 || c.Host != "0.0.0.0" || c.DataDir != "/app/data" || c.BaseURL != "https://www.animeworld.ac" ||
		c.LogLevel != "INFO" || c.MaxSeries != 8 || c.Concurrency != 4 || c.SearchBudget != 40*time.Second ||
		c.DownloadDir != "/downloads" {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	env := map[string]string{
		"API_KEY": "k", "PROXY_PORT": "1234", "DATA_DIR": "/d", "ANIMEWORLD_URL": "https://x.test/",
		"FLARESOLVERR_URL": "http://byparr:8191", "LOG_LEVEL": "debug", "SEARCH_BUDGET": "5s",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "k" || c.Port != 1234 || c.DataDir != "/d" || c.BaseURL != "https://x.test" ||
		c.FlareSolverrURL != "http://byparr:8191" || c.LogLevel != "DEBUG" || c.SearchBudget != 5*time.Second {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestBadValuesAreErrors(t *testing.T) {
	for k, v := range map[string]string{"PROXY_PORT": "abc", "CONCURRENCY": "0", "SEARCH_BUDGET": "soon", "MAX_SERIES": "-1"} {
		if _, err := Load(func(key string) string {
			if key == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s should be rejected", k, v)
		}
	}
}
