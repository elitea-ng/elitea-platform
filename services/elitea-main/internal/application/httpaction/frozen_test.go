package httpaction

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type frozenFixture struct {
	Cases []struct {
		Name        string `json:"name"`
		Numeric     string `json:"numeric_token"`
		Application struct {
			ID        int64 `json:"id"`
			VersionID int64 `json:"version_id"`
			Version   struct {
				Instructions string         `json:"instructions"`
				Snapshot     FrozenSnapshot `json:"http_action_snapshot"`
			} `json:"version_details"`
		} `json:"application"`
		Wire          string `json:"request_wire_utf8"`
		Binding       string `json:"binding_digest"`
		RequestDigest string `json:"request_digest"`
	} `json:"cases"`
}

func loadFrozenFixture(t *testing.T) frozenFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/http_frozen_request.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture frozenFixture
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid fixture")
	}
	return fixture
}
func TestHTTPMainFreezeMatchesExactCrossLanguageNumericTokens(t *testing.T) {
	fixture := loadFrozenFixture(t)
	for _, f := range fixture.Cases {
		t.Run(f.Name, func(t *testing.T) {
			snapshot, err := FreezeSnapshot(f.Application.ID, f.Application.VersionID, f.Application.Version.Instructions)
			if err != nil {
				t.Fatal(err)
			}
			node := snapshot.Nodes[0]
			wire, err := base64.StdEncoding.DecodeString(node.RequestWireB64)
			if err != nil || string(wire) != f.Wire || node.BindingDigest != f.Binding || node.RequestDigest != f.RequestDigest {
				t.Fatal("Main emitted a different exact request")
			}
			var payload map[string]json.RawMessage
			var body struct {
				Value map[string]json.RawMessage `json:"value"`
			}
			_ = json.Unmarshal(wire, &payload)
			_ = json.Unmarshal(payload["body"], &body)
			if string(body.Value["n"]) != f.Numeric {
				t.Fatal("numeric source token changed")
			}
		})
	}
	if fixture.Cases[0].RequestDigest == fixture.Cases[1].RequestDigest {
		t.Fatal("integer and decimal conflated")
	}
	if fixture.Cases[0].RequestDigest != fixture.Cases[3].RequestDigest {
		t.Fatal("Main key ordering unstable")
	}
	if fixture.Cases[0].Binding == fixture.Cases[3].Binding {
		t.Fatal("immutable authored instructions not bound")
	}
}
func TestHTTPMainFreezeDeniesAmbiguousYAMLAndUnsupportedNumericForms(t *testing.T) {
	source := loadFrozenFixture(t).Cases[0].Application.Version.Instructions
	for _, bad := range []string{
		strings.Replace(source, "n: 1", "n: .nan", 1), strings.Replace(source, "n: 1", "n: 0x11", 1),
		strings.Replace(source, "n: 1", "n: 01", 1), strings.Replace(source, "a: 2, n: 1", "n: 1, n: 2", 1),
		strings.Replace(source, "{a: 2, n: 1}", "&value {a: 2, n: 1}", 1),
		source + "---\nnodes: []\n",
	} {
		if _, err := FreezeSnapshot(31, 41, bad); err == nil {
			t.Fatal("ambiguous YAML admitted")
		}
	}
}
func TestHTTPFrozenSelectionRequestAndReplayTamperingHaveZeroAdmission(t *testing.T) {
	f := loadFrozenFixture(t).Cases[0]
	snapshot := f.Application.Version.Snapshot
	inv := Invocation{SchemaVersion: Schema, NodeID: "fetch", ThreadID: "graph", BindingDigest: f.Binding, RequestDigest: f.RequestDigest, RequestWireB64: snapshot.Nodes[0].RequestWireB64}
	inv.ActivationID = VisitID(inv.ThreadID, inv.NodeID, 0, BindingBytes(inv.BindingDigest))
	if snapshot.Verify(31, 41, f.Application.Version.Instructions, inv) != nil {
		t.Fatal("owner receipt rejected")
	}
	for _, change := range []func(*Invocation){
		func(i *Invocation) {
			i.RequestWireB64 = base64.StdEncoding.EncodeToString([]byte(strings.Replace(f.Wire, `"n":1`, `"n":1.0`, 1)))
			i.RequestDigest = Digest([]byte(strings.Replace(f.Wire, `"n":1`, `"n":1.0`, 1)))
		},
		func(i *Invocation) { i.BindingDigest = strings.Repeat("a", 64) }, func(i *Invocation) { i.NodeID = "other" },
		func(i *Invocation) { i.RequestWireB64 += "\n" },
	} {
		changed := inv
		change(&changed)
		if snapshot.Verify(31, 41, f.Application.Version.Instructions, changed) == nil {
			t.Fatal("tampered replay admitted")
		}
	}
	if snapshot.Verify(31, 42, f.Application.Version.Instructions, inv) == nil || snapshot.Verify(32, 41, f.Application.Version.Instructions, inv) == nil || snapshot.Verify(31, 41, f.Application.Version.Instructions+"#edit", inv) == nil {
		t.Fatal("immutable selection changed")
	}
}
