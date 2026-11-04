// Package wrap fills text to a fixed line width.
package wrap

import (
	"errors"
	"strings"
)

// Wrap greedily fills text into lines of at most width runes. Paragraphs
// (separated by blank lines) are preserved and joined by one empty line.
func Wrap(text string, width int) (string, error) {
	if width < 1 {
		return "", errors.New("wrap: width must be at least 1")
	}
	var paragraphs []string
	var words []string
	flush := func() {
		if len(words) > 0 {
			paragraphs = append(paragraphs, strings.Join(fill(words, width), "\n"))
			words = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			flush()
			continue
		}
		words = append(words, fields...)
	}
	flush()
	return strings.Join(paragraphs, "\n\n"), nil
}

// fill lays out words greedily; widths are measured in runes.
func fill(words []string, width int) []string {
	var lines []string
	var cur []rune
	for _, w := range words {
		r := []rune(w)
		if len(cur) > 0 && len(cur)+1+len(r) <= width {
			cur = append(cur, ' ')
			cur = append(cur, r...)
			continue
		}
		if len(cur) > 0 {
			lines = append(lines, string(cur))
		}
		for len(r) > width {
			lines = append(lines, string(r[:width]))
			r = r[width:]
		}
		cur = append([]rune(nil), r...)
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return lines
}
