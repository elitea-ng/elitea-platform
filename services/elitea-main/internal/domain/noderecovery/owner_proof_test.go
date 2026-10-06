package noderecovery

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNodeRecoveryOwnerProofExactScopeAndResultReference(t *testing.T) {
	r := Receipt{Schema: Schema, ActivationID: strings.Repeat("1", 64), JournalRevision: 3, NodeID: "fetch", GraphThread: "root", Step: 7, Attempt: 1, FailureClass: "dependency_unavailable", StopReason: "effect_reconciliation_required", ReplaySafety: ReplaySafety{Kind: "unknown_external_effect", EffectID: strings.Repeat("3", 64)}, AllowedActions: []string{"reconcile"}}
	p := OwnerProof{Schema: "elitea.pipeline.node-recovery-owner-proof.v1", Kind: "committed_result", ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationID: r.ActivationID, Attempt: 1, ExpectedRevision: 3, EffectID: r.ReplaySafety.EffectID, OwnerReceiptSHA256: strings.Repeat("4", 64), ResultRef: &ResultReference{ContentID: r.ReplaySafety.EffectID, ImmutableVersion: strings.Repeat("4", 64), DigestSHA256: strings.Repeat("4", 64), ByteLength: 64, MediaType: "application/json", RequiredGrantAudience: HTTPResultAudience}}
	raw, _ := json.Marshal(p)
	if _, err := DecodeOwnerProof(raw, p.ExecutionID, 1, r, "reconcile"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*OwnerProof)
	}{{"foreign execution", func(p *OwnerProof) { p.ExecutionID = "fedcba9876543210fedcba9876543210" }}, {"generation", func(p *OwnerProof) { p.Generation = 2 }}, {"activation", func(p *OwnerProof) { p.ActivationID = strings.Repeat("9", 64) }}, {"revision", func(p *OwnerProof) { p.ExpectedRevision = 4 }}, {"attempt", func(p *OwnerProof) { p.Attempt = 2 }}, {"other effect", func(p *OwnerProof) { p.EffectID = strings.Repeat("9", 64) }}, {"missing ref", func(p *OwnerProof) { p.ResultRef = nil }}, {"content relation", func(p *OwnerProof) { p.ResultRef.ContentID = strings.Repeat("9", 64) }}, {"version relation", func(p *OwnerProof) { p.ResultRef.ImmutableVersion = strings.Repeat("9", 64) }}, {"digest relation", func(p *OwnerProof) { p.ResultRef.DigestSHA256 = strings.Repeat("9", 64) }}, {"audience", func(p *OwnerProof) { p.ResultRef.RequiredGrantAudience = "arbitrary" }}, {"no effect with result", func(p *OwnerProof) { p.Kind = "verified_no_effect" }}} {
		t.Run(tc.name, func(t *testing.T) {
			q := p
			ref := *p.ResultRef
			q.ResultRef = &ref
			tc.change(&q)
			b, _ := json.Marshal(q)
			if _, err := DecodeOwnerProof(b, p.ExecutionID, 1, r, "reconcile"); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, mutation := range []string{strings.Replace(string(raw), `"kind":`, `"extra":true,"kind":`, 1), strings.Replace(string(raw), `"generation":1`, `"generation":1,"generation":1`, 1), strings.Replace(string(raw), `"result_ref":{`, `"result_ref":{"extra":true,`, 1), strings.Replace(string(raw), `"result_ref":{`, `"result_ref":null,"ignored":{`, 1)} {
		if _, err := DecodeOwnerProof([]byte(mutation), p.ExecutionID, 1, r, "reconcile"); err == nil {
			t.Fatal("accepted malformed")
		}
	}
	if _, err := DecodeOwnerProof(raw, p.ExecutionID, 1, r, "retry"); err == nil {
		t.Fatal("committed proof permitted retry")
	}
}
