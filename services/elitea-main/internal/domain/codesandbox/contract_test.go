package codesandbox

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func codeFixture() (Binding, Visit) {
	activation := strings.Repeat("1", 64)
	effect := DispatchActivation(activation, 1)
	return Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: "whole_code_execute", ExecutionID: "0123456789abcdef0123456789abcdef", OriginalGeneration: 1, ActivationID: activation, NodeID: "run", GraphThread: "root", Step: 1, Attempt: 1, DispatchActivation: effect, JobKey: JobKey("0123456789abcdef0123456789abcdef", effect), RequestDigest: strings.Repeat("2", 64), SupervisorAudience: "spiffe://elitea/supervisor/one", NodeDigest: strings.Repeat("3", 64), Language: "python", PreparedSHA256: strings.Repeat("4", 64), SourceSHA256: Digest([]byte("7")), InputSHA256: Digest([]byte("{}"))}, Visit{ActivationID: activation, NodeID: "run", GraphThread: "root", Step: 1, Attempt: 1, ExpectedRevision: 2, ReceiptSHA256: strings.Repeat("5", 64)}
}
func TestWholeCodeCommittedReceiptStrictCanonicalAndExactOwner(t *testing.T) {
	b, v := codeFixture()
	result := []byte(`{"output":7}`)
	r := Receipt{Schema: "elitea.sandbox.whole-code-recovery-receipt.v1", Kind: "committed_result", Binding: b, Visit: v, ResultBase64URL: base64.RawURLEncoding.EncodeToString(result), ResultSHA256: Digest(result)}
	wire, _ := Canonical(r)
	if _, err := DecodeReceipt(wire, b, v); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Receipt)
	}{
		{"different input", func(r *Receipt) { r.Binding.InputSHA256 = strings.Repeat("9", 64) }},
		{"different generation", func(r *Receipt) { r.Binding.OriginalGeneration++ }},
		{"different effect", func(r *Receipt) { r.Binding.DispatchActivation = strings.Repeat("9", 64) }},
		{"different visit", func(r *Receipt) { r.Visit.ExpectedRevision++ }},
		{"padding", func(r *Receipt) { r.ResultBase64URL += "=" }},
		{"result tamper", func(r *Receipt) { r.ResultSHA256 = strings.Repeat("9", 64) }},
		{"kind smuggling", func(r *Receipt) { r.SealID = strings.Repeat("9", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := r
			tc.change(&changed)
			raw, _ := Canonical(changed)
			if _, err := DecodeReceipt(raw, b, v); err == nil {
				t.Fatal("accepted altered receipt")
			}
		})
	}
	if _, err := DecodeReceipt(append(bytes.Clone(wire), '\n'), b, v); err == nil {
		t.Fatal("accepted noncanonical newline")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(wire, &fields)
	fields["arbitrary_child"] = json.RawMessage(`"child"`)
	changed, _ := json.Marshal(fields)
	if _, err := DecodeReceipt(changed, b, v); err == nil {
		t.Fatal("child selector accepted")
	}
}
func TestCodeVisitSyntaxKeepsRuntimeIDsAndRejectsAuthoritySmuggling(t *testing.T) {
	b, v := codeFixture()
	b.ExecutionID = strings.Repeat("0", 32)
	b.JobKey = JobKey(b.ExecutionID, b.DispatchActivation)
	if b.Validate() != nil {
		t.Fatal("syntactic zero runtime ID rejected")
	}
	for _, id := range []string{"10000000-0000-4000-8000-000000000001", strings.Repeat("A", 32), strings.Repeat("0", 31)} {
		b.ExecutionID = id
		if b.Validate() == nil {
			t.Fatal("accepted nonproduction execution form")
		}
	}
	if !VisitBounds(v.ActivationID, v.NodeID, v.GraphThread, v.Step, v.Attempt) {
		t.Fatal("valid visit bounds refused")
	}
	for _, thread := range []string{"root\nchild", "root\u2028child", strings.Repeat("é", 257)} {
		if VisitBounds(v.ActivationID, v.NodeID, thread, v.Step, v.Attempt) {
			t.Fatal("invalid graph thread admitted")
		}
	}
	var target IntentRequest
	raw := []byte(`{"schema":"x","schema":"y"}`)
	if Decode(raw, &target, 4096) == nil {
		t.Fatal("duplicate accepted")
	}
	if RequiredFields([]byte(`{"required":null}`), []string{"required"}) {
		t.Fatal("null nonnullable accepted")
	}
}
