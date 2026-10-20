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
	for _, blocked := range []error{animeworld.ErrBlocked, animeworld.ErrOriginBlocked} {
		err := fmt.Errorf("search: %w", blocked)
		rec := get(newHandler(&fakeBackend{err: err}), "/api?t=search&q=x&apikey=secret")
		var f feed
		if rec.Code != http.StatusOK || xml.Unmarshal(rec.Body.Bytes(), &f) != nil || len(f.Items) != 0 {
			t.Fatalf("%v: code=%d items=%d body=%s", blocked, rec.Code, len(f.Items), rec.Body)
		}
	}
}
