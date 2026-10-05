package httpcache

import "testing"

func TestETagMatches(t *testing.T) {
	const etag = `"abc"`
	for _, tc := range []struct {
		inm  string
		want bool
	}{
		{"", false},
		{`"abc"`, true},
		{`W/"abc"`, true},
		{`*`, true},
		{` * `, true},
		{`"x", "abc"`, true},
		{`"x","y"`, false},
		{`"abc,*,def"`, false},
		{`"x", *`, false},
	} {
		if got := ETagMatches(tc.inm, etag); got != tc.want {
			t.Errorf("ETagMatches(%q) = %v, want %v", tc.inm, got, tc.want)
		}
	}
}

func TestStrongETag(t *testing.T) {
	quoted, value := StrongETag([]byte("x"))
	if quoted != `"`+value+`"` || len(value) != 64 {
		t.Fatalf("StrongETag = %q, %q", quoted, value)
	}
	if q2, _ := StrongETag([]byte("x")); q2 != quoted {
		t.Fatal("not deterministic")
	}
}
