package delegationgrant

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The vectors are shared with the worker (libs/rust/agent-runtime
// delegation_tests.rs runs the same file). The expected canonical text and
// digest were produced independently of both implementations.
func TestDigestVectorsAgreeWithTheWorker(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/delegation-grant-digest-vectors-v1.json"))
	if err != nil {
		t.Fatalf("read the vectors: %v", err)
	}
	var vectors struct {
		Cases []struct {
			Name      string          `json:"name"`
			GrantSet  json.RawMessage `json:"grant_set"`
			Canonical string          `json:"canonical"`
			Digest    string          `json:"digest"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) < 7 {
		t.Fatalf("only %d vectors", len(vectors.Cases))
	}
	for _, c := range vectors.Cases {
		t.Run(c.Name, func(t *testing.T) {
			set, err := ParseGrantSet(c.GrantSet)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := string(set.Canonical()); got != c.Canonical {
				t.Fatalf("canonical\n got %s\nwant %s", got, c.Canonical)
			}
			if got := set.Digest(); got != c.Digest {
				t.Fatalf("digest = %s, want %s", got, c.Digest)
			}
			// The canonical form parses back to the same set and digest.
			again, err := ParseGrantSet(set.Canonical())
			if err != nil || again.Digest() != c.Digest {
				t.Fatalf("round trip: %v %s", err, again.Digest())
			}
		})
	}
}

// A callee's toolkit references build the same grant set in Main and in the
// worker (GrantSet::from_callee).
func TestFromToolsMatchesTheWorkerVectors(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/delegation-grant-digest-vectors-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		ToolsCases []struct {
			Name        string           `json:"name"`
			Tools       []map[string]any `json:"tools"`
			DataClasses []string         `json:"data_classes"`
			Canonical   string           `json:"canonical"`
			Digest      string           `json:"digest"`
		} `json:"tools_cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.ToolsCases) < 5 {
		t.Fatalf("only %d tools vectors", len(vectors.ToolsCases))
	}
	for _, c := range vectors.ToolsCases {
		t.Run(c.Name, func(t *testing.T) {
			var tools []Tool
			for _, raw := range c.Tools {
				tool := Tool{Type: raw["type"].(string)}
				if id, ok := raw["id"].(float64); ok {
					tool.ID = int64(id)
				}
				tool.Settings, _ = raw["settings"].(map[string]any)
				tools = append(tools, tool)
			}
			got := FromTools(tools, c.DataClasses)
			if string(got.Canonical()) != c.Canonical || got.Digest() != c.Digest {
				t.Fatalf("canonical\n got %s\nwant %s", got.Canonical(), c.Canonical)
			}
		})
	}
}

func set(toolkits ...ToolkitGrant) GrantSet { return GrantSet{Toolkits: toolkits} }

func only(key string, credential string, tools ...string) ToolkitGrant {
	return ToolkitGrant{Key: key, Credential: credential, Tools: Tools{Mode: ToolsOnly, Names: tools}}
}

func all(key string, except ...string) ToolkitGrant {
	return ToolkitGrant{Key: key, Tools: Tools{Mode: ToolsAllExcept, Names: except}}
}

func TestMissingIsSetContainmentNotVersionEquality(t *testing.T) {
	grant := GrantSet{
		Toolkits:    []ToolkitGrant{only("toolkit:1", "ref-a", "read", "write"), all("toolkit:2", "drop")},
		DataClasses: []string{"internal"},
		EgressHosts: []string{"git.example.com"},
	}
	cases := []struct {
		name    string
		current GrantSet
		want    []Item
	}{
		{"the same set", grant, nil},
		{"a smaller set", GrantSet{Toolkits: []ToolkitGrant{only("toolkit:1", "ref-a", "read")}}, nil},
		{"a new toolkit", GrantSet{Toolkits: []ToolkitGrant{only("toolkit:1", "ref-a", "read"), all("toolkit:3")}},
			[]Item{{Kind: ItemToolkit, Name: "toolkit:3"}}},
		{"a new tool", set(only("toolkit:1", "ref-a", "read", "delete")),
			[]Item{{Kind: ItemTool, Name: "delete", Toolkit: "toolkit:1"}}},
		{"a swapped credential", set(only("toolkit:1", "ref-b", "read")),
			[]Item{{Kind: ItemCredential, Name: "ref-b", Toolkit: "toolkit:1"}}},
		{"a toolkit that grew to every tool", set(all("toolkit:1")),
			[]Item{{Kind: ItemTool, Name: "*", Toolkit: "toolkit:1"}}},
		{"a new data class", GrantSet{DataClasses: []string{"internal", "pii"}},
			[]Item{{Kind: ItemDataClass, Name: "pii"}}},
		{"a new host", GrantSet{EgressHosts: []string{"db.example.com"}},
			[]Item{{Kind: ItemEgressHost, Name: "db.example.com"}}},
		{"every tool where the grant excluded one", set(all("toolkit:2")),
			[]Item{{Kind: ItemTool, Name: "drop", Toolkit: "toolkit:2"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := grant.Missing(c.current)
			if len(got) != len(c.want) {
				t.Fatalf("missing = %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("missing[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

type fixedAccess struct{ toolkits, credentials map[string]bool }

func (a fixedAccess) HoldsToolkit(key string) bool             { return a.toolkits[key] }
func (a fixedAccess) HoldsCredential(_, reference string) bool { return a.credentials[reference] }

func TestApprovableLeavesOutWhatTheApproverDoesNotHold(t *testing.T) {
	requested := GrantSet{
		Toolkits: []ToolkitGrant{
			only("toolkit:1", "ref-git", "read", "write"),
			all("toolkit:2"),
			only("toolkit:3", "ref-db", "query"),
		},
		DataClasses: []string{"internal"},
		EgressHosts: []string{"git.example.com"},
	}
	access := fixedAccess{
		toolkits:    map[string]bool{"toolkit:1": true, "toolkit:2": true, "toolkit:3": true},
		credentials: map[string]bool{"ref-git": true}, // not ref-db
	}
	approved, omitted, err := Approvable(requested, nil, access)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved.Toolkits) != 2 || approved.Toolkits[0].Key != "toolkit:1" || approved.Toolkits[1].Key != "toolkit:2" {
		t.Fatalf("approved toolkits: %+v", approved.Toolkits)
	}
	if !requested.Contains(approved) || approved.Contains(requested) {
		t.Fatal("the approved set must be a strict part of the requested set")
	}
	left := map[Item]bool{}
	for _, item := range omitted {
		left[item] = true
	}
	for _, want := range []Item{
		{Kind: ItemToolkit, Name: "toolkit:3"},
		{Kind: ItemCredential, Name: "ref-db", Toolkit: "toolkit:3"},
		{Kind: ItemTool, Name: "query", Toolkit: "toolkit:3"},
	} {
		if !left[want] {
			t.Fatalf("omitted lacks %+v: %+v", want, omitted)
		}
	}
	if approved.Digest() == requested.Digest() {
		t.Fatal("a partial approval has its own digest")
	}
}

func TestApprovableHonoursTheChosenItems(t *testing.T) {
	requested := set(only("toolkit:1", "", "read", "write"), all("toolkit:2"))
	access := fixedAccess{toolkits: map[string]bool{"toolkit:1": true, "toolkit:2": true}}
	approved, omitted, err := Approvable(requested, []Item{
		{Kind: ItemToolkit, Name: "toolkit:1"},
		{Kind: ItemTool, Name: "read", Toolkit: "toolkit:1"},
	}, access)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved.Toolkits) != 1 || len(approved.Toolkits[0].Tools.Names) != 1 || approved.Toolkits[0].Tools.Names[0] != "read" {
		t.Fatalf("approved: %+v", approved)
	}
	if len(omitted) != 2 { // the write tool and toolkit:2
		t.Fatalf("omitted: %+v", omitted)
	}
	if _, _, err := Approvable(requested, []Item{}, access); !errors.Is(err, ErrNothingToApprove) {
		t.Fatalf("choosing nothing: %v", err)
	}
	if _, _, err := Approvable(requested, nil, fixedAccess{}); !errors.Is(err, ErrNothingToApprove) {
		t.Fatalf("holding nothing: %v", err)
	}
}

func TestParseGrantSetRefusesWhatTheWorkerCouldNotRead(t *testing.T) {
	for name, doc := range map[string]string{
		"not json":            `x`,
		"a wrong schema":      `{"schema":"x","toolkits":[],"data_classes":null,"egress_hosts":[]}`,
		"a bad key":           `{"schema":"elitea.delegation.grants.v1","toolkits":[{"key":"jira","tools":{"only":[]},"credential":null}],"data_classes":null,"egress_hosts":[]}`,
		"a bad mode":          `{"schema":"elitea.delegation.grants.v1","toolkits":[{"key":"toolkit:1","tools":{"some":[]},"credential":null}],"data_classes":null,"egress_hosts":[]}`,
		"null tools":          `{"schema":"elitea.delegation.grants.v1","toolkits":[{"key":"toolkit:1","tools":{"only":null},"credential":null}],"data_classes":null,"egress_hosts":[]}`,
		"an empty credential": `{"schema":"elitea.delegation.grants.v1","toolkits":[{"key":"toolkit:1","tools":{"only":[]},"credential":""}],"data_classes":null,"egress_hosts":[]}`,
		"a bad data class":    `{"schema":"elitea.delegation.grants.v1","toolkits":[],"data_classes":["Bad Class"],"egress_hosts":[]}`,
		"trailing data":       `{"schema":"elitea.delegation.grants.v1","toolkits":[],"data_classes":null,"egress_hosts":[]} x`,
		"an unknown member":   `{"schema":"elitea.delegation.grants.v1","toolkits":[],"data_classes":null,"egress_hosts":[],"x":1}`,
	} {
		if _, err := ParseGrantSet([]byte(doc)); !errors.Is(err, ErrMalformedSet) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestMakerChecker(t *testing.T) {
	proposer := int64(7)
	if err := CheckApprover(true, &proposer, 7); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("governed self-approval: %v", err)
	}
	if err := CheckApprover(true, nil, 8); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("governed, no proposer: %v", err)
	}
	if err := CheckApprover(true, &proposer, 8); err != nil {
		t.Fatalf("governed, a second person: %v", err)
	}
	if err := CheckApprover(false, &proposer, 7); err != nil {
		t.Fatalf("outside governed an editor approves their own: %v", err)
	}
}

func TestStateMachine(t *testing.T) {
	for _, c := range []struct {
		from, to State
		ok       bool
	}{
		{StatePending, StateApproved, true},
		{StatePending, StateStale, false},
		{StateApproved, StateStale, true},
		{StateStale, StateSuperseded, true},
		{StateStale, StateApproved, true},
		{StateRevoked, StateApproved, false},
		{StateSuperseded, StateRevoked, false},
	} {
		if CanTransition(c.from, c.to) != c.ok {
			t.Errorf("%s -> %s: want %v", c.from, c.to, c.ok)
		}
	}
	if !StateStale.Consumable() || StatePending.Consumable() || StateRevoked.Active() {
		t.Fatal("consumable and active")
	}
}
