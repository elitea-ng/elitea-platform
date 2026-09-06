package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCreateEgressAllowlistAcceptsTheGrammar proves the write path admits every
// shape the gateway can enforce. The parser is the SHARED one
// (libs/go/egresslib), so this surface and the gateway cannot disagree about
// what a valid entry is.
func TestCreateEgressAllowlistAcceptsTheGrammar(t *testing.T) {
	body := `{"type":"egress_allowlist","name":"on-prem","enabled":true,"data":{"egress":{"allowlist":` +
		`["vllm.ml.svc.cluster.local:8000","192.168.29.60:8000","192.168.29.0/24","*.openai.azure.com"]}}}`
	q := &fakeQuerier{rowResult: okRow("e1", "egress_allowlist", "on-prem", nil, true)}
	h := NewGovernanceHandler(q)

	rr := doJSON(h, http.MethodPost, "/governance", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var row GovernanceRow
	_ = json.NewDecoder(rr.Body).Decode(&row)
	if row.ID != "e1" {
		t.Fatalf("id = %q, want e1", row.ID)
	}
}

// TestCreateEgressAllowlistRejectsAMalformedEntry: a host this form accepted and
// the gateway silently dropped would leave an operator looking at a saved rule
// while their provider keeps failing. The check runs server side and does not
// depend on what the client validated.
func TestCreateEgressAllowlistRejectsAMalformedEntry(t *testing.T) {
	cases := []struct {
		name string
		list string
	}{
		{"a scheme", `["https://vllm.internal/v1"]`},
		{"a path", `["vllm.internal/v1"]`},
		{"a bare wildcard", `["*"]`},
		{"a wildcard in the middle", `["ev*il.example"]`},
		{"a bad port", `["host.example:notaport"]`},
		{"a bad CIDR", `["192.168.0.0/33"]`},
		{"an empty entry", `[""]`},
		{"a blank entry", `["   "]`},
		{"a non-string entry", `[42]`},
		// An empty list is not the empty mcp_allowlist, which turns that
		// control off. This one permits nothing and widens nothing.
		{"an empty list", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{rowResult: okRow("x", "egress_allowlist", "bad", nil, true)}
			h := NewGovernanceHandler(q)
			body := `{"type":"egress_allowlist","name":"bad","data":{"egress":{"allowlist":` + tc.list + `}}}`

			rr := doJSON(h, http.MethodPost, "/governance", body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s; body=%s", rr.Code, tc.name, rr.Body.String())
			}
			if q.lastSQL != "" && strings.Contains(q.lastSQL, "INSERT") {
				t.Fatal("a refused entry still reached the database")
			}
		})
	}
}

// TestCreateEgressAllowlistRejectsAScope: half of what the row governs — whether
// bifrost's SSRF-safe dialer is relaxed — is decided with no project in hand, so
// a scoped row could only be half-honoured. Applying it globally would be wider
// than it was authored, so the write is refused instead.
func TestCreateEgressAllowlistRejectsAScope(t *testing.T) {
	for _, scope := range []string{
		`{"project_ids":[7]}`,
		`{"providers":["openai"]}`,
		`{"models":["gpt-4o"]}`,
	} {
		q := &fakeQuerier{rowResult: okRow("x", "egress_allowlist", "scoped", nil, true)}
		h := NewGovernanceHandler(q)
		body := `{"type":"egress_allowlist","name":"scoped","data":{"scope":` + scope +
			`,"egress":{"allowlist":["a.example.com"]}}}`

		rr := doJSON(h, http.MethodPost, "/governance", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for scope %s; body=%s", rr.Code, scope, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "global") {
			t.Fatalf("the refusal does not tell the operator that the entry is global: %s", rr.Body.String())
		}
	}
}

// TestUpdateEgressAllowlistValidatesToo: the update path must hold the same rule
// as the create path, or an entry that could not be created can be edited into
// existence.
func TestUpdateEgressAllowlistValidatesToo(t *testing.T) {
	q := &fakeQuerier{rowResult: okRow("x", "egress_allowlist", "e", nil, true)}
	h := NewGovernanceHandler(q)
	body := `{"type":"egress_allowlist","name":"e","data":{"egress":{"allowlist":["https://nope/path"]}}}`

	rr := doJSON(h, http.MethodPut, "/governance/x", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestEgressAllowlistAcceptsTheFlatShape covers a hand-written row and any older
// client, exactly as the gateway's compiler does.
func TestEgressAllowlistAcceptsTheFlatShape(t *testing.T) {
	if err := validateEgressAllowlistData(map[string]any{
		"allowlist": []any{"a.example.com"},
	}); err != nil {
		t.Fatalf("the flat shape was refused: %v", err)
	}
}

// TestOtherTypesKeepTheirScope proves the global-scope rule is confined to the
// egress row. Every other type is scoped on purpose.
func TestOtherTypesKeepTheirScope(t *testing.T) {
	q := &fakeQuerier{rowResult: okRow("b", "budget", "scoped-budget", nil, true)}
	h := NewGovernanceHandler(q)
	body := `{"type":"budget","name":"scoped-budget","data":{"scope":{"project_ids":[7]},"budget":{"limit_usd":5}}}`

	rr := doJSON(h, http.MethodPost, "/governance", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a project-scoped budget must still be authorable. body=%s",
			rr.Code, rr.Body.String())
	}
}
