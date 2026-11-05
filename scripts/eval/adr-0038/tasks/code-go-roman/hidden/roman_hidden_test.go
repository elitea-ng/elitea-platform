package roman

import "testing"

func TestHiddenRoundTripAll(t *testing.T) {
	seen := make(map[string]int, 3999)
	for n := 1; n <= 3999; n++ {
		s, err := ToRoman(n)
		if err != nil {
			t.Fatalf("ToRoman(%d): %v", n, err)
		}
		if prev, dup := seen[s]; dup {
			t.Fatalf("ToRoman(%d) = %q, same as ToRoman(%d)", n, s, prev)
		}
		seen[s] = n
		back, err := FromRoman(s)
		if err != nil || back != n {
			t.Fatalf("FromRoman(%q) = %d, %v; want %d", s, back, err, n)
		}
	}
}

func TestHiddenRejectsAllNonCanonicalShortStrings(t *testing.T) {
	// Every string of up to 4 numeral letters that is not a canonical numeral
	// must be rejected.
	canonical := map[string]bool{}
	for n := 1; n <= 3999; n++ {
		s, _ := ToRoman(n)
		canonical[s] = true
	}
	letters := "IVXLCDM"
	var gen func(prefix string, depth int)
	gen = func(prefix string, depth int) {
		if prefix != "" && !canonical[prefix] {
			if got, err := FromRoman(prefix); err == nil {
				t.Errorf("FromRoman(%q) = %d, want error", prefix, got)
			}
		}
		if depth == 0 {
			return
		}
		for i := 0; i < len(letters); i++ {
			gen(prefix+string(letters[i]), depth-1)
		}
	}
	gen("", 4)
}

func TestHiddenRejectsMixedCaseAndUnicode(t *testing.T) {
	for _, s := range []string{"mcmxciv", "McMXCIV", "Ⅻ", "XⅠ", "\u0000I", "I\n"} {
		if got, err := FromRoman(s); err == nil {
			t.Errorf("FromRoman(%q) = %d, want error", s, got)
		}
	}
}
