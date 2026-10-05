package noderecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestNodeRecoveryStrictReceiptCanonicalVector(t *testing.T) {
	raw, err := os.ReadFile("../../../../../libs/jsonschema/runtime/v1/fixtures/node-recovery-required-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := DecodeReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ActivationID != strings.Repeat("1", 64) || receipt.JournalRevision != 3 || receipt.AllowedActions[0] != "retry" {
		t.Fatal(receipt)
	}
	canonical, err := CanonicalReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if got := hex.EncodeToString(digest[:]); got != "8f592df47f70a2b2f5c4bf0bd97832856cd194d139a0eeab7dc37a82ca4b1351" {
		t.Fatal(got)
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("fixture")
	}
	for name, mutate := range map[string]func(map[string]any){
		"denial":            func(m map[string]any) { m["failure_class"] = "authorization_denied" },
		"cancel":            func(m map[string]any) { m["failure_class"] = "cancelled" },
		"lease lost":        func(m map[string]any) { m["failure_class"] = "lease_lost" },
		"unknown":           func(m map[string]any) { m["failure_class"] = "unknown" },
		"zero activation":   func(m map[string]any) { m["activation_id"] = strings.Repeat("0", 64) },
		"uppercase":         func(m map[string]any) { m["activation_id"] = strings.Repeat("A", 64) },
		"revision null":     func(m map[string]any) { m["journal_revision"] = nil },
		"missing zero step": func(m map[string]any) { delete(m, "step") },
		"max attempts":      func(m map[string]any) { m["attempt"] = 17 },
		"terminal reason":   func(m map[string]any) { m["stop_reason"] = "attempts_exhausted" },
		"wrong action":      func(m map[string]any) { m["allowed_actions"] = []string{"reconcile"} },
		"extra selector":    func(m map[string]any) { m["child_thread_id"] = "foreign" },
		"line separator":    func(m map[string]any) { m["graph_thread"] = "a\u2028b" },
		"thread control":    func(m map[string]any) { m["graph_thread"] = "a\nb" },
		"thread bytes":      func(m map[string]any) { m["graph_thread"] = strings.Repeat("é", 257) },
		"nested null": func(m map[string]any) {
			m["replay_safety"] = map[string]any{"kind": "no_external_effect", "effect_id": nil}
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyFields := map[string]any{}
			for k, v := range fields {
				copyFields[k] = v
			}
			mutate(copyFields)
			bad, _ := json.Marshal(copyFields)
			if _, err := DecodeReceipt(bad); err == nil {
				t.Fatal(string(bad))
			}
		})
	}
	duplicate := strings.Replace(string(raw), `"journal_revision":3`, `"journal_revision":3,"journal_revision":3`, 1)
	if _, err := DecodeReceipt([]byte(duplicate)); err == nil {
		t.Fatal("duplicate admitted")
	}
}

func TestNodeRecoveryStrictRequestAndEffectActions(t *testing.T) {
	request := Request{RequestID: strings.Repeat("2", 64), ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationID: strings.Repeat("1", 64), ExpectedRevision: 3, Action: "retry"}
	raw, _ := json.Marshal(request)
	if _, err := DecodeRequest(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.TrimSuffix(string(raw), "}") + `,"child_thread_id":"foreign"}`, strings.Replace(string(raw), `"generation":1`, `"generation":1,"generation":1`, 1), strings.Replace(string(raw), `"action":"retry"`, `"action":null`, 1), strings.Replace(string(raw), `"action":"retry"`, `"action":"authorize"`, 1), strings.Replace(string(raw), `"generation":1`, `"generation":9223372036854775808`, 1)} {
		if _, err := DecodeRequest([]byte(bad)); err == nil {
			t.Fatal(bad)
		}
	}
	for _, spec := range []struct{ kind, action, field string }{{"unknown_external_effect", "reconcile", "effect_id"}, {"completed_external_effect", "resume_result", "receipt_id"}, {"unclassified", "reconcile", ""}} {
		r := Receipt{Schema: Schema, ActivationID: strings.Repeat("1", 64), JournalRevision: 3, NodeID: "fetch", GraphThread: "thread", Attempt: 1, FailureClass: "invalid_result", StopReason: "effect_reconciliation_required", ReplaySafety: ReplaySafety{Kind: spec.kind}, AllowedActions: []string{spec.action}}
		if spec.field == "effect_id" {
			r.ReplaySafety.EffectID = strings.Repeat("3", 64)
		}
		if spec.field == "receipt_id" {
			r.ReplaySafety.ReceiptID = strings.Repeat("4", 64)
		}
		if err := r.Validate(); err != nil {
			t.Fatal(spec, err)
		}
		r.AllowedActions = []string{"retry"}
		if r.Validate() == nil {
			t.Fatal("ambiguous effect repeated")
		}
	}
}
