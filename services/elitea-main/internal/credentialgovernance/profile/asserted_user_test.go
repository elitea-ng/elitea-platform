package profile

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// Gate D4: absent, no session carries asserted user evidence; set, only the
// listed modes do, and each must also be an allowed identity mode.
func TestAssertedUserIdentityModesDefaultToNone(t *testing.T) {
	doc, canonical, err := ParsePolicyDocument([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range agentidentity.IdentityModes() {
		if doc.AllowsAssertedUser(mode) {
			t.Fatalf("an absent list must allow no asserted user, allowed %q", mode)
		}
	}
	if strings.Contains(string(canonical), "asserted") {
		t.Fatalf("an absent list must not change the canonical bytes: %s", canonical)
	}
	set := strings.Replace(validPolicy, `}`, `,"asserted_user_identity_modes":["dedicated_agent"]}`, 1)
	if doc, _, err = ParsePolicyDocument([]byte(set)); err != nil {
		t.Fatal(err)
	}
	if !doc.AllowsAssertedUser(agentidentity.IdentityModeDedicatedAgent) ||
		doc.AllowsAssertedUser(agentidentity.IdentityModeRuntimeDomainBinding) {
		t.Fatal("the list is not read")
	}
	for name, extra := range map[string]string{
		"mode not allowed": `,"asserted_user_identity_modes":["elitea_native"]}`,
		"unknown mode":     `,"asserted_user_identity_modes":["somebody"]}`,
		"repeated mode":    `,"asserted_user_identity_modes":["dedicated_agent","dedicated_agent"]}`,
	} {
		raw := strings.Replace(validPolicy, `}`, extra, 1)
		if _, _, err := ParsePolicyDocument([]byte(raw)); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: err = %v, want ErrInvalidPolicy", name, err)
		}
	}
}
