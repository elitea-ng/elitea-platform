package skilltrust

import "testing"

func TestForeign(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		parent        string
		project       int64
		wantPublisher int64
		wantForeign   bool
	}{
		{"no lineage", "", 7, 0, false},
		{"null lineage", "null", 7, 0, false},
		{"forked from another project", "1", 7, 1, true},
		{"parent is this project", "7", 7, 0, false},
		{"malformed lineage reads as foreign", "abc", 7, 0, true},
		{"zero parent reads as foreign", "0", 7, 0, true},
		{"negative parent reads as foreign", "-3", 7, 0, true},
		{"whitespace", " 1 ", 7, 1, true},
	}
	for _, tc := range cases {
		publisher, foreign := Foreign(tc.parent, tc.project)
		if publisher != tc.wantPublisher || foreign != tc.wantForeign {
			t.Errorf("%s: Foreign(%q, %d) = (%d, %v), want (%d, %v)",
				tc.name, tc.parent, tc.project, publisher, foreign, tc.wantPublisher, tc.wantForeign)
		}
	}
}

func TestTrustedIsPerRevision(t *testing.T) {
	t.Parallel()
	const text = "Review the diff."
	trusted := []string{Revision(text)}

	if Trusted(text, nil) {
		t.Fatal("a foreign skill with no trust record is not trusted")
	}
	if !Trusted(text, trusted) {
		t.Fatal("a foreign skill whose current revision is trusted is trusted")
	}
	if Trusted(text+" Then merge it.", trusted) {
		t.Fatal("a changed text is a new revision and is untrusted again")
	}
}

// The digest is the value the worker freezes; a drift would make every trust
// record miss.
func TestRevisionIsTheHexSHA256(t *testing.T) {
	t.Parallel()
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256("")
	if got := Revision(""); got != want {
		t.Fatalf("Revision(\"\") = %s, want %s", got, want)
	}
}
