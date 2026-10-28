package profile

import (
	"errors"
	"strings"
	"testing"
)

// Gate D4 (#1122): absent, no unattended governed run may start; set, only
// the two mandate kinds are accepted, once each.
func TestUnattendedMandateKindsDefaultToNone(t *testing.T) {
	doc, canonical, err := ParsePolicyDocument([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if doc.AllowsUnattendedMandateKind("autonomous") || doc.AllowsUnattendedMandateKind("delegated_background") {
		t.Fatal("an absent list must allow no unattended mandate kind")
	}
	if strings.Contains(string(canonical), "unattended") {
		t.Fatalf("an absent list must not change the canonical bytes: %s", canonical)
	}
	set := strings.Replace(validPolicy, `}`, `,"unattended_mandate_kinds":["delegated_background"]}`, 1)
	if doc, _, err = ParsePolicyDocument([]byte(set)); err != nil {
		t.Fatal(err)
	}
	if !doc.AllowsUnattendedMandateKind("delegated_background") || doc.AllowsUnattendedMandateKind("autonomous") {
		t.Fatal("the list is not read")
	}
	for name, extra := range map[string]string{
		"unknown kind":  `,"unattended_mandate_kinds":["interactive_delegation"]}`,
		"repeated kind": `,"unattended_mandate_kinds":["autonomous","autonomous"]}`,
	} {
		raw := strings.Replace(validPolicy, `}`, extra, 1)
		if _, _, err := ParsePolicyDocument([]byte(raw)); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: err = %v, want ErrInvalidPolicy", name, err)
		}
	}
}
