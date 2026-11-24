package httpaction

import (
	"encoding/base64"
	"strings"
	"testing"
)

const frozenPipeline = `entry_point: fetch
nodes:
  - id: fetch
    type: http
    revision: 2
    operation: tracker.create_item
    request:
      path: {item: {from: item_key}}
      query: {q: {value: "all"}}
      headers: [{name: X-Trace-Id, from: trace}]
      body: {kind: json, value: {a: 2, n: 1}}
      idempotency_key: {from: request_id}
    response: {mode: json, accepted_statuses: [{first: 200, last: 299}], max_bytes: 1048576}
    timeout_ms: 15000
    output: [ticket]
    transition: END
`

func frozenInvocation(t *testing.T, snapshot *FrozenSnapshot, inputs string) Invocation {
	t.Helper()
	raw, err := CanonicalJSON([]byte(inputs))
	if err != nil {
		t.Fatal(err)
	}
	inv := Invocation{SchemaVersion: Schema, NodeID: "fetch", ThreadID: "graph", Step: 3, BindingDigest: snapshot.Nodes[0].BindingDigest, InputsB64: base64.StdEncoding.EncodeToString(raw), InputsDigest: Digest(raw)}
	inv.ActivationID = VisitID(inv.ThreadID, inv.NodeID, inv.Step, BindingBytes(inv.BindingDigest))
	return inv
}

func TestFreezeSnapshotV2FreezesTemplateAndRuleNameOnly(t *testing.T) {
	snapshot, err := FreezeSnapshot(31, 41, frozenPipeline)
	if err != nil || snapshot == nil || len(snapshot.Nodes) != 1 {
		t.Fatalf("freeze: %v %v", snapshot, err)
	}
	node := snapshot.Nodes[0]
	wire, err := base64.StdEncoding.DecodeString(node.TemplateWireB64)
	if err != nil || Digest(wire) != node.TemplateDigest || node.Operation != "tracker.create_item" || node.Output[0] != "ticket" || *node.Transition != "END" {
		t.Fatalf("frozen node %+v", node)
	}
	const want = `{"operation":"tracker.create_item","request":{"body":{"kind":"json","value":{"a":2,"n":1}},"headers":[{"from":"trace","name":"X-Trace-Id"}],"idempotency_key":{"from":"request_id"},"path":{"item":{"from":"item_key"}},"query":{"q":{"value":"all"}}},"response":{"accepted_statuses":[{"first":200,"last":299}],"max_bytes":1048576,"mode":"json"},"timeout_ms":15000}`
	if string(wire) != want {
		t.Fatalf("template wire\n got %s\nwant %s", wire, want)
	}
	// Byte-identical source freezes byte-identically; an edit rebinds.
	again, _ := FreezeSnapshot(31, 41, frozenPipeline)
	edited, _ := FreezeSnapshot(31, 41, frozenPipeline+"# edit\n")
	if again.Nodes[0].BindingDigest != node.BindingDigest || edited.Nodes[0].BindingDigest == node.BindingDigest || edited.Nodes[0].TemplateDigest != node.TemplateDigest {
		t.Fatal("binding digest does not bind the exact saved instructions")
	}
	// Integer and decimal YAML tokens stay distinct (no float round trip).
	decimal, err := FreezeSnapshot(31, 41, strings.Replace(frozenPipeline, "n: 1}", "n: 1.0}", 1))
	if err != nil || decimal.Nodes[0].TemplateDigest == node.TemplateDigest {
		t.Fatalf("integer and decimal conflated: %v", err)
	}
}

func TestFreezeSnapshotRefusesRevisionOneAndAmbiguousYAML(t *testing.T) {
	for name, bad := range map[string]string{
		"revision 1":      strings.Replace(frozenPipeline, "revision: 2", "revision: 1", 1),
		"url in node":     strings.Replace(frozenPipeline, "    timeout_ms: 15000\n", "    timeout_ms: 15000\n    url: https://x.example\n", 1),
		"method in node":  strings.Replace(frozenPipeline, "    timeout_ms: 15000\n", "    timeout_ms: 15000\n    method: GET\n", 1),
		"nan":             strings.Replace(frozenPipeline, "n: 1}", "n: .nan}", 1),
		"hex":             strings.Replace(frozenPipeline, "n: 1}", "n: 0x11}", 1),
		"leading zero":    strings.Replace(frozenPipeline, "n: 1}", "n: 01}", 1),
		"duplicate key":   strings.Replace(frozenPipeline, "{a: 2, n: 1}", "{n: 1, n: 2}", 1),
		"anchor in value": strings.Replace(frozenPipeline, "{a: 2, n: 1}", "&value {a: 2, n: 1}", 1),
		"multi-document":  frozenPipeline + "---\nnodes: []\n",
		"no operation":    strings.Replace(frozenPipeline, "    operation: tracker.create_item\n", "", 1),
		"bad operation":   strings.Replace(frozenPipeline, "operation: tracker.create_item", "operation: Tracker/Create", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FreezeSnapshot(31, 41, bad); err == nil {
				t.Fatal("admitted")
			}
		})
	}
}

func TestFrozenVerifySelectsTemplateAndRefusesTampering(t *testing.T) {
	snapshot, err := FreezeSnapshot(31, 41, frozenPipeline)
	if err != nil {
		t.Fatal(err)
	}
	inv := frozenInvocation(t, snapshot, `{"headers":{"x-trace-id":"t"},"idempotency_key":"k1","path":{"item":"i"}}`)
	template, err := snapshot.Verify(31, 41, frozenPipeline, inv)
	if err != nil || template.Operation != "tracker.create_item" {
		t.Fatalf("verify: %v", err)
	}
	for name, change := range map[string]func(*Invocation){
		"binding": func(i *Invocation) { i.BindingDigest = strings.Repeat("a", 64) },
		"node":    func(i *Invocation) { i.NodeID = "other" },
	} {
		changed := inv
		change(&changed)
		if _, err := snapshot.Verify(31, 41, frozenPipeline, changed); err == nil {
			t.Fatalf("%s tampering admitted", name)
		}
	}
	if _, err := snapshot.Verify(31, 42, frozenPipeline, inv); err == nil {
		t.Fatal("other version admitted")
	}
	if _, err := snapshot.Verify(32, 41, frozenPipeline, inv); err == nil {
		t.Fatal("other application admitted")
	}
	if _, err := snapshot.Verify(31, 41, frozenPipeline+"#edit", inv); err == nil {
		t.Fatal("edited instructions admitted")
	}
	tampered := *snapshot
	tampered.Nodes = append([]FrozenNode(nil), snapshot.Nodes...)
	tampered.Nodes[0].TemplateWireB64 = base64.StdEncoding.EncodeToString([]byte(`{"operation":"other.rule","request":{},"response":{"mode":"json"}}`))
	if _, err := tampered.Verify(31, 41, frozenPipeline, inv); err == nil {
		t.Fatal("swapped template admitted")
	}
}

func TestParseInvocationBoundsAndIdentity(t *testing.T) {
	snapshot, _ := FreezeSnapshot(31, 41, frozenPipeline)
	inv := frozenInvocation(t, snapshot, `{"path":{"item":"i"}}`)
	if _, err := ParseInvocation(inv); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		change func(*Invocation)
		code   string
	}{
		"schema":         {func(i *Invocation) { i.SchemaVersion = "elitea.runtime.http-action.v2" }, CodeInvalidInput},
		"digest":         {func(i *Invocation) { i.InputsDigest = strings.Repeat("b", 64) }, CodeInvalidInput},
		"activation":     {func(i *Invocation) { i.Step++ }, CodeInvalidInput},
		"attempt 16":     {func(i *Invocation) { i.Attempt = MaxAttempts }, CodeInvalidInput},
		"base64 padding": {func(i *Invocation) { i.InputsB64 += "=" }, CodeInvalidInput},
		"scope_ref": {func(i *Invocation) {
			i.ScopeRef = &ScopeRef{ScopeID: strings.Repeat("a", 64), Revision: 1, DigestSHA256: strings.Repeat("b", 64)}
		}, CodePolicyDenied},
		"oversized b64": {func(i *Invocation) {
			i.InputsB64 = strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxInputsBytes)+4)
		}, CodeInputsTooBig},
	} {
		changed := inv
		tc.change(&changed)
		if _, err := ParseInvocation(changed); FailureCode(err) != tc.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	last := inv
	last.Attempt = MaxAttempts - 1
	if _, err := ParseInvocation(last); err != nil {
		t.Fatalf("attempt 15 refused: %v", err)
	}
}
