package requestlog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Truncate is shared by the request log and both usage-ledger writers
// (internal/failmode usageEventArgs). The cut must land on a rune boundary, at
// the exact length, for 2-, 3- and 4-byte runes straddling the limit.
func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		limit int
		want  int // bytes kept
	}{
		{"short", "abc", 8, 3},
		{"exact", "abcd", 4, 4},
		{"ascii", "abcdef", 4, 4},
		{"2-byte straddle", "abc" + "é", 4, 3},
		{"3-byte straddle", "ab" + "€", 4, 2},
		{"4-byte straddle", "a" + "😀", 4, 1},
		{"4-byte ends at the limit", "😀" + "x", 4, 4},
		{"3-byte boundary", "€€", 3, 3},
		{"zero limit", "abc", 0, 0},
		{"empty", "", 4, 0},
	} {
		got := Truncate(tc.in, tc.limit)
		if len(got) != tc.want || !utf8.ValidString(got) || !strings.HasPrefix(tc.in, got) {
			t.Errorf("%s: %q -> %q (%d bytes, valid=%v), want %d valid bytes",
				tc.name, tc.in, got, len(got), utf8.ValidString(got), tc.want)
		}
	}
}
