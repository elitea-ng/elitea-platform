// Package roman converts between integers and Roman numerals.
package roman

import (
	"fmt"
	"strings"
)

var table = []struct {
	value  int
	symbol string
}{
	{1000, "M"}, {900, "CM"}, {500, "D"}, {400, "CD"},
	{100, "C"}, {90, "XC"}, {50, "L"}, {40, "XL"},
	{10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"},
}

// ToRoman returns the canonical Roman numeral for n (1..3999).
func ToRoman(n int) (string, error) {
	if n < 1 || n > 3999 {
		return "", fmt.Errorf("roman: %d out of range 1..3999", n)
	}
	var b strings.Builder
	for _, e := range table {
		for n >= e.value {
			b.WriteString(e.symbol)
			n -= e.value
		}
	}
	return b.String(), nil
}

var values = map[byte]int{'I': 1, 'V': 5, 'X': 10, 'L': 50, 'C': 100, 'D': 500, 'M': 1000}

// FromRoman parses a canonical upper-case Roman numeral in 1..3999.
//
// The numeral is evaluated with the usual subtractive rule and then checked
// against the canonical rendering of the result, which rejects every
// non-canonical spelling (IIII, IC, VX, MMMM, ...).
func FromRoman(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("roman: empty numeral")
	}
	total := 0
	for i := 0; i < len(s); i++ {
		v, ok := values[s[i]]
		if !ok {
			return 0, fmt.Errorf("roman: invalid character %q in %q", s[i], s)
		}
		if i+1 < len(s) && values[s[i+1]] > v {
			total -= v
		} else {
			total += v
		}
	}
	canonical, err := ToRoman(total)
	if err != nil || canonical != s {
		return 0, fmt.Errorf("roman: %q is not a canonical numeral", s)
	}
	return total, nil
}
