package roman

import "testing"

func TestToRomanKnownValues(t *testing.T) {
	cases := map[int]string{
		1: "I", 3: "III", 4: "IV", 9: "IX", 14: "XIV", 40: "XL", 49: "XLIX",
		90: "XC", 94: "XCIV", 400: "CD", 444: "CDXLIV", 1994: "MCMXCIV",
		2026: "MMXXVI", 3999: "MMMCMXCIX",
	}
	for n, want := range cases {
		got, err := ToRoman(n)
		if err != nil || got != want {
			t.Errorf("ToRoman(%d) = %q, %v; want %q", n, got, err, want)
		}
	}
}

func TestToRomanRange(t *testing.T) {
	for _, n := range []int{0, -1, 4000, 5000} {
		if got, err := ToRoman(n); err == nil {
			t.Errorf("ToRoman(%d) = %q, want error", n, got)
		}
	}
}

func TestFromRomanKnownValues(t *testing.T) {
	cases := map[string]int{
		"I": 1, "IV": 4, "IX": 9, "XLII": 42, "XCIX": 99, "CDXLIV": 444,
		"MCMXCIV": 1994, "MMMCMXCIX": 3999,
	}
	for s, want := range cases {
		got, err := FromRoman(s)
		if err != nil || got != want {
			t.Errorf("FromRoman(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
}

func TestFromRomanRejectsRepeats(t *testing.T) {
	for _, s := range []string{"IIII", "VV", "XXXX", "LL", "DD", "MMMM"} {
		if got, err := FromRoman(s); err == nil {
			t.Errorf("FromRoman(%q) = %d, want error", s, got)
		}
	}
}

func TestFromRomanRejectsBadSubtraction(t *testing.T) {
	for _, s := range []string{"IC", "IL", "XM", "XD", "VX", "IIX", "XCX", "IXI", "CMC"} {
		if got, err := FromRoman(s); err == nil {
			t.Errorf("FromRoman(%q) = %d, want error", s, got)
		}
	}
}

func TestFromRomanRejectsJunk(t *testing.T) {
	for _, s := range []string{"", "iv", "Xii", "ABC", "X I", "IV "} {
		if got, err := FromRoman(s); err == nil {
			t.Errorf("FromRoman(%q) = %d, want error", s, got)
		}
	}
}

func TestRoundTripSample(t *testing.T) {
	for _, n := range []int{1, 8, 19, 39, 89, 99, 499, 999, 1888, 2444, 3888} {
		s, err := ToRoman(n)
		if err != nil {
			t.Fatalf("ToRoman(%d): %v", n, err)
		}
		back, err := FromRoman(s)
		if err != nil || back != n {
			t.Errorf("FromRoman(ToRoman(%d) = %q) = %d, %v", n, s, back, err)
		}
	}
}
