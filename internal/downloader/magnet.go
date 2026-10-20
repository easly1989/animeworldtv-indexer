package downloader

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// The hand-off between the Torznab feed and the emulated qBittorrent client is
// a magnet URI. Its info-hash is derived from the episode token (Sonarr tracks
// the download by that hash) and the token itself rides in the custom "aw"
// parameter, so the client never needs a lookup table to know what to fetch.

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidToken reports whether s looks like an AnimeWorld episode token.
func ValidToken(s string) bool { return tokenRe.MatchString(s) }

// Hash is the 40-char lowercase hex info-hash standing for an episode.
func Hash(token string) string {
	sum := sha1.Sum([]byte("animeworld:" + token))
	return hex.EncodeToString(sum[:])
}

// Magnet builds the magnet URI for an episode; name becomes its display name.
func Magnet(token, name string) string {
	// %20 rather than "+": magnet parsers do not all decode the latter.
	dn := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return "magnet:?xt=urn:btih:" + Hash(token) + "&dn=" + dn + "&aw=" + token
}

// ParseMagnet extracts the episode token and display name from a magnet URI
// built by Magnet. It rejects anything else, including a hash that does not
// match the token (Sonarr would lose track of such a download).
func ParseMagnet(raw string) (token, name string, err error) {
	if !strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		return "", "", errors.New("not a magnet link")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	token = q.Get("aw")
	if !ValidToken(token) {
		return "", "", errors.New("magnet does not carry an AnimeWorld episode")
	}
	if xt := strings.ToLower(q.Get("xt")); xt != "urn:btih:"+Hash(token) {
		return "", "", errors.New("magnet hash does not match its episode")
	}
	return token, q.Get("dn"), nil
}
