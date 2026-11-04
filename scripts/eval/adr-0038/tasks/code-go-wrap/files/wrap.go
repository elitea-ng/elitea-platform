// Package wrap fills text to a fixed line width.
package wrap

import (
	"errors"
	"strings"
)

// Wrap greedily fills text into lines of at most width characters.
func Wrap(text string, width int) (string, error) {
	if width < 0 {
		return "", errors.New("wrap: width must be positive")
	}
	words := strings.Fields(text)
	var lines []string
	line := ""
	for _, w := range words {
		if line == "" {
			line = w
			continue
		}
		if len(line)+1+len(w) >= width {
			lines = append(lines, line)
			line = w
		} else {
			line += " " + w
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}
