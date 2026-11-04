package wrap

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func mustWrap(t *testing.T, text string, width int) string {
	t.Helper()
	got, err := Wrap(text, width)
	if err != nil {
		t.Fatalf("Wrap(%q, %d) error: %v", text, width, err)
	}
	return got
}

func check(t *testing.T, text string, width int, want string) {
	t.Helper()
	if got := mustWrap(t, text, width); got != want {
		t.Errorf("Wrap(%q, %d)\n got: %q\nwant: %q", text, width, got, want)
	}
}

func TestGreedyFill(t *testing.T) {
	check(t, "the quick brown fox jumps over the lazy dog", 10,
		"the quick\nbrown fox\njumps over\nthe lazy\ndog")
}

func TestLineOfExactlyWidthFits(t *testing.T) {
	check(t, "ab cd", 5, "ab cd")
	check(t, "ab cd ef", 5, "ab cd\nef")
}

func TestCountsRunesNotBytes(t *testing.T) {
	check(t, "héllo wörld", 11, "héllo wörld")
	check(t, "日本語 テキスト です", 8, "日本語 テキスト\nです")
}

func TestLongWordIsBroken(t *testing.T) {
	check(t, "abcdefghij", 4, "abcd\nefgh\nij")
	check(t, "hi abcdefghij x", 4, "hi\nabcd\nefgh\nij x")
	check(t, "ééééé", 2, "éé\néé\né")
}

func TestParagraphsPreserved(t *testing.T) {
	check(t, "one two\nthree\n\n\n  \t\nfour five", 20, "one two three\n\nfour five")
	check(t, "\n\n  lead\n\ntrail  \n\n", 20, "lead\n\ntrail")
}

func TestWhitespaceCollapsesAndCRLF(t *testing.T) {
	check(t, "  a\t\tb \n c  ", 10, "a b c")
	check(t, "a\r\n\r\nb", 10, "a\n\nb")
}

func TestEmptyInput(t *testing.T) {
	check(t, "", 10, "")
	check(t, " \n\t\n ", 10, "")
}

func TestInvalidWidth(t *testing.T) {
	for _, w := range []int{0, -3} {
		if _, err := Wrap("text", w); err == nil {
			t.Errorf("Wrap with width %d should fail", w)
		}
	}
}

func TestInvariantsOnLongText(t *testing.T) {
	text := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor. ", 6)
	for width := 5; width <= 40; width++ {
		got := mustWrap(t, text, width)
		for _, line := range strings.Split(got, "\n") {
			if n := utf8.RuneCountInString(line); n > width || n == 0 {
				t.Fatalf("width %d: bad line %q (%d runes)", width, line, n)
			}
			if strings.TrimSpace(line) != line {
				t.Fatalf("width %d: line %q has surrounding spaces", width, line)
			}
		}
		if width >= 12 && strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(text), " ") {
			t.Fatalf("width %d: words changed", width)
		}
	}
}
