package clientversion

import "testing"

func TestParseAcceptsAndRefuses(t *testing.T) {
	good := []string{"1.2.3", "v1.2.3", "0.0.0", "1.2.3-beta", "1.2.3-rc.1", "1.2.3+42", "1.2.3-rc.1+sha.abc", "01.2.3"}
	for _, text := range good {
		if !Valid(text) {
			t.Errorf("Valid(%q) = false, want true", text)
		}
	}
	bad := []string{"", "1", "1.2", "1.2.3.4", "a.b.c", "1.2.x", "1.2.3-", "1.2.3+", "1.2.3-a..b",
		"1.2.3-b@d", " 1.2.3", "1.2.3 ", "-1.2.3", "1.2.3-é", "1234567890.0.0",
		"1.2.3-" + string(make([]byte, 70))}
	for _, text := range bad {
		if Valid(text) {
			t.Errorf("Valid(%q) = true, want false", text)
		}
	}
}

func TestComparePrecedence(t *testing.T) {
	// SemVer 2.0.0 §11's own example chain, plus build metadata and v.
	chain := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0",
	}
	for i := 0; i+1 < len(chain); i++ {
		a, _ := Parse(chain[i])
		b, _ := Parse(chain[i+1])
		if Compare(a, b) != -1 || Compare(b, a) != 1 {
			t.Errorf("want %s < %s", chain[i], chain[i+1])
		}
	}
	a, _ := Parse("v1.2.3+build.7")
	b, _ := Parse("1.2.3")
	if Compare(a, b) != 0 {
		t.Errorf("build metadata and v must not affect precedence")
	}
	c, _ := Parse("1.0.0-007")
	d, _ := Parse("1.0.0-7")
	if Compare(c, d) != 0 {
		t.Errorf("numeric identifiers compare by value")
	}
}

func TestBelow(t *testing.T) {
	cases := []struct {
		version, minimum string
		below, err       bool
	}{
		{"1.0.0", "", false, false},
		{"garbage", "", false, false},
		{"0.9.9", "1.0.0", true, false},
		{"1.0.0", "1.0.0", false, false},
		{"1.0.0-rc.1", "1.0.0", true, false},
		{"1.0.1", "1.0.0", false, false},
		{"garbage", "1.0.0", false, true},
		{"1.0.0", "garbage", false, true},
	}
	for _, tc := range cases {
		below, err := Below(tc.version, tc.minimum)
		if below != tc.below || (err != nil) != tc.err {
			t.Errorf("Below(%q, %q) = %v, %v; want %v, err=%v", tc.version, tc.minimum, below, err, tc.below, tc.err)
		}
	}
}

func TestMax(t *testing.T) {
	cases := [][3]string{
		{"1.0.0", "2.0.0", "2.0.0"},
		{"2.0.0", "1.0.0", "2.0.0"},
		{"", "1.0.0", "1.0.0"},
		{"1.0.0", "", "1.0.0"},
		{"", "", ""},
		{"bad", "1.0.0", "1.0.0"},
	}
	for _, tc := range cases {
		if got := Max(tc[0], tc[1]); got != tc[2] {
			t.Errorf("Max(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}
