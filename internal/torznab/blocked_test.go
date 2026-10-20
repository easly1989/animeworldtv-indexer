package torznab

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"testing"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
)

// A Cloudflare block must give Prowlarr an empty feed, not an error.
func TestSearchBlockedByCloudflareIsEmpty(t *testing.T) {
	err := fmt.Errorf("search: %w", animeworld.ErrBlocked)
	rec := get(newHandler(&fakeBackend{err: err}), "/api?t=search&q=x&apikey=secret")
	var f feed
	if rec.Code != http.StatusOK || xml.Unmarshal(rec.Body.Bytes(), &f) != nil || len(f.Items) != 0 {
		t.Fatalf("code=%d items=%d body=%s", rec.Code, len(f.Items), rec.Body)
	}
}
