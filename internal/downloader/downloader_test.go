package downloader

import (
	"strings"
	"testing"
)

func TestMagnetRoundTrip(t *testing.T) {
	m := Magnet("vlhlNp", "Frieren & Co - 01 [SUB ITA]")
	if strings.Contains(m, "+") || !strings.Contains(m, "btih:"+Hash("vlhlNp")) {
		t.Fatalf("magnet %q", m)
	}
	token, name, err := ParseMagnet(m)
	if err != nil || token != "vlhlNp" || name != "Frieren & Co - 01 [SUB ITA]" {
		t.Errorf("got %q %q %v", token, name, err)
	}
	if h := Hash("vlhlNp"); len(h) != 40 || h != strings.ToLower(h) || h == Hash("NFYNE-") {
		t.Errorf("hash %q", h)
	}
	if _, _, err := ParseMagnet(strings.Replace(m, "aw=vlhlNp", "aw=NFYNE-", 1)); err == nil {
		t.Error("a magnet whose hash and token disagree must be rejected")
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"Show - 01 [SUB ITA]": "Show - 01 [SUB ITA]",
		"../../etc/passwd":    "_.._etc_passwd",
		`a:b*c?"d<e>f|g\h`:    "a_b_c__d_e_f_g_h",
		"  ..  ":              "",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeName(strings.Repeat("é", 300)); len(got) > 200 || !strings.HasPrefix(got, "é") {
		t.Errorf("long name not bounded: %d bytes", len(got))
	}
}
