package agentproposal

import "testing"

// Rust's str::to_lowercase for the cases Go's strings.ToLower differs on
// (#1861).
func TestRuntimeLowerMatchesRust(t *testing.T) {
	for input, want := range map[string]string{
		"GitHub":   "github",
		"ΟΔΟΣ":     "οδος",
		"ΣΑΣ":      "σας",
		"Σ":        "σ",
		"ΑΣ.Β":     "ασ.β",
		"Α.Σ":      "α.ς",
		"ΑΣ'Β":     "ασ'β",
		"ΑΣ1":      "ας1",
		"İSTANBUL": "i̇stanbul",
		"Почта":    "почта",
	} {
		if got := RuntimeLower(input); got != want {
			t.Errorf("RuntimeLower(%q) = %q (%U), want %q (%U)", input, got, []rune(got), want, []rune(want))
		}
	}
}

// The cross-language vector of a profile whose tool keys need Rust's
// lowering (the runtime's a_profile_digest_matches_mains_vectors pins the
// same digest): each half of a key is lowered on its own, so the sigma at
// the end of the toolkit is final.
func TestSpecDigestLowersNonASCIIKeysAsTheRuntime(t *testing.T) {
	checked, err := Check(Request{EphemeralAgentID: "a_0000002a", Rationale: "why", Profile: Profile{
		Soul:  "s",
		Tools: []Tool{{Key: "ΟΔΟΣ/ΣΑΣ"}, {Key: "İKIT/*"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if checked.Profile.Tools[0].Key != "οδος/σας" {
		t.Fatalf("checked key %q", checked.Profile.Tools[0].Key)
	}
	if got := SpecDigest(checked.Profile); got != nonASCIIProfileDigest {
		t.Fatalf("non-ASCII profile digest %s", got)
	}
	// An unchecked profile digests the same.
	raw := Profile{Soul: "s", Tools: []Tool{{Key: "İKIT/*"}, {Key: " ΟΔΟΣ / ΣΑΣ "}}}
	if got := SpecDigest(raw); got != nonASCIIProfileDigest {
		t.Fatalf("raw non-ASCII profile digest %s", got)
	}
}

const nonASCIIProfileDigest = "9a3ad9dce6052b88b11d55abce4abd384544a6d566802f31835a48292978d265"
