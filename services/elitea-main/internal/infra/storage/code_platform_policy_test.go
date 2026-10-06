package storage

import (
	"encoding/json"
	"testing"
)

func TestCodeBrokerPolicyMatchesExactWorkerBytesAndDigest(t *testing.T) {
	policy := CodeBrokerPolicy{Revision: 1, MaxCalls: 32, MaxTotalBytes: 1048576}
	raw, err := json.Marshal(policy)
	if err != nil || string(raw) != `{"revision":1,"max_calls":32,"max_total_bytes":1048576}` {
		t.Fatal(string(raw), err)
	}
	digest, err := policy.SHA256()
	if err != nil || digest != "a299856c1b28b5aee8488da91e8dd2cb33251c89b677733b63447179144d03da" {
		t.Fatal(digest, err)
	}
	changed := policy
	changed.MaxCalls++
	other, _ := changed.SHA256()
	if other == digest {
		t.Fatal("changed operator policy retained identity")
	}
	if _, err = NewCodeBrokerPolicies([]CodeBrokerPolicy{policy, policy}); err == nil {
		t.Fatal("duplicate policy admitted")
	}
	for _, invalid := range []CodeBrokerPolicy{{Revision: 0, MaxCalls: 32, MaxTotalBytes: 1048576}, {Revision: 1, MaxCalls: 0, MaxTotalBytes: 1}, {Revision: 1, MaxCalls: 4097, MaxTotalBytes: 1}, {Revision: 1, MaxCalls: 1, MaxTotalBytes: 0}, {Revision: 1, MaxCalls: 1, MaxTotalBytes: 67108865}} {
		if _, err = invalid.SHA256(); err == nil {
			t.Fatal("invalid policy admitted")
		}
	}
}

func TestCodeBrokerPolicyGrammarNeverSelectsUnconfiguredOperatorPolicy(t *testing.T) {
	zero := "0000000000000000000000000000000000000000000000000000000000000000"
	if !workspaceHex(zero, 64) {
		t.Fatal("prepared policy grammar changed")
	}
	for _, invalid := range []string{"", zero[:63], "A" + zero[1:]} {
		if workspaceHex(invalid, 64) {
			t.Fatal("malformed policy digest admitted")
		}
	}
	policies, err := NewCodeBrokerPolicies([]CodeBrokerPolicy{{Revision: 1, MaxCalls: 32, MaxTotalBytes: 1048576}})
	if err != nil {
		t.Fatal(err)
	}
	if _, configured := policies.policies[zero]; configured {
		t.Fatal("syntactic policy became operator authority")
	}
}
