package web

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"pademelon/internal/clocks"
)

// TestUIRefreshMatchesEmbeddedHTML keeps REFRESH_MS in the page and
// clocks.UIRefresh from drifting apart. The page is embedded statically and
// can't read Go constants, so the constant mirrors it — this test is the
// enforcement. Update both or neither.
func TestUIRefreshMatchesEmbeddedHTML(t *testing.T) {
	re := regexp.MustCompile(`const REFRESH_MS = (\d+);`)
	m := re.FindSubmatch(indexHTML)
	if m == nil {
		t.Fatal("REFRESH_MS constant not found in index.html")
	}
	ms, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("REFRESH_MS is not a number: %v", err)
	}
	if got := time.Duration(ms) * time.Millisecond; got != clocks.UIRefresh {
		t.Fatalf("index.html REFRESH_MS (%s) does not match clocks.UIRefresh (%s); update both",
			got, clocks.UIRefresh)
	}
}
