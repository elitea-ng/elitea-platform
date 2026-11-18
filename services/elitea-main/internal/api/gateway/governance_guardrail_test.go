package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

// gatewayGuardrailCases is the gateway's case file. The gateway runs ParseRow
// over it; this test runs the write-side check over it, so the two agree.
const gatewayGuardrailCases = "../../../../elitea-llm-gateway/internal/guardrail/testdata/rows.json"

func TestGuardrailValidationMatchesTheGatewayCases(t *testing.T) {
	raw, err := os.ReadFile(gatewayGuardrailCases)
	if err != nil {
		t.Fatalf("read the gateway's guardrail cases: %v (if the file moved, update the path; do not delete the guard)", err)
	}
	var doc struct {
		Cases []struct {
			Name  string         `json:"name"`
			Valid bool           `json:"valid"`
			Data  map[string]any `json:"data"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 10 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		err := validateGovernanceRow(GovernanceRow{Type: guardrailType, Name: "g", Data: c.Data})
		if c.Valid && err != nil {
			t.Errorf("%s: refused a row the gateway accepts: %v", c.Name, err)
		}
		if !c.Valid && err == nil {
			t.Errorf("%s: accepted a row the gateway rejects", c.Name)
		}
	}
}

// TestGuardrailSecretRuleIDsArePinned keeps the rule ids equal to the rule
// set the gateway's secrets detector embeds.
func TestGuardrailSecretRuleIDsArePinned(t *testing.T) {
	raw, err := os.ReadFile("../../../../../libs/rust/local-tools/src/handoff/secret_rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rules []struct {
			ID string `json:"id"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range doc.Rules {
		ids = append(ids, r.ID)
	}
	want := append([]string(nil), guardrailSecretRuleIDs...)
	sort.Strings(ids)
	sort.Strings(want)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("guardrailSecretRuleIDs = %v, the rule set has %v", want, ids)
	}
}

func TestGuardrailScopeAcceptsAgentsButNotUsers(t *testing.T) {
	data := func(scope map[string]any) map[string]any {
		return map[string]any{"scope": scope, "guardrail": map[string]any{
			"stage": "output", "detectors": []any{map[string]any{"type": "secrets", "action": "mask"}},
		}}
	}
	if err := validateGovernanceRow(GovernanceRow{Type: guardrailType, Name: "g",
		Data: data(map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0}})}); err != nil {
		t.Fatalf("an agent-scoped guardrail was refused: %v", err)
	}
	if err := validateGovernanceRow(GovernanceRow{Type: guardrailType, Name: "g",
		Data: data(map[string]any{"user_ids": []any{3.0}})}); err == nil {
		t.Fatal("a user-scoped guardrail was accepted")
	}
	if err := validateGovernanceRow(GovernanceRow{Type: "budget", Name: "b",
		Data: map[string]any{"scope": map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0}}}}); err == nil {
		t.Fatal("an agent-scoped budget was accepted")
	}
}

// TestGuardrailTopLevelShapeIsRefusedWith400 pins the one data shape. The
// admin form writes data.guardrail and reads only data.guardrail. A row with
// stage and detectors at the top level of data would show defaults in the
// form, and a save would overwrite the authored values. So the write refuses
// it with a 400 that names data.guardrail. The validation runs before any
// database call, so the handler needs no pool here.
func TestGuardrailTopLevelShapeIsRefusedWith400(t *testing.T) {
	body := `{"type":"guardrail","name":"g","data":{"stage":"output","detectors":[{"type":"secrets","action":"mask"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/governance", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	(&GovernanceHandler{}).Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data.guardrail") {
		t.Fatalf("the refusal does not name data.guardrail: %s", rec.Body.String())
	}
	for name, data := range map[string]map[string]any{
		"not an object": {"guardrail": "output"},
		"absent":        {},
	} {
		if err := validateGuardrailData(data); err == nil || !strings.Contains(err.Error(), "data.guardrail") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestGuardrailPersonalDataRuleIDsMatchTheGateway pins the write-side list of
// personal_data rule ids to the gateway's own constants (issue 2012). The two
// modules cannot import each other, so the gateway's source is read.
func TestGuardrailPersonalDataRuleIDsMatchTheGateway(t *testing.T) {
	raw, err := os.ReadFile("../../../../elitea-llm-gateway/internal/guardrail/personal_data.go")
	if err != nil {
		t.Fatalf("read the gateway's personal data rules: %v (if the file moved, update the path; do not delete the guard)", err)
	}
	source := string(raw)
	for _, id := range guardrailPersonalDataRuleIDs {
		if !strings.Contains(source, `"`+id+`"`) {
			t.Errorf("the gateway has no personal data rule %q", id)
		}
	}
	// The gateway's list is the four ids: a rule added there needs the same here.
	if got := strings.Count(source, "PersonalData"); got == 0 {
		t.Fatal("the gateway's personal data constants moved")
	}
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PersonalData") && strings.Contains(line, "= \"") {
			id := line[strings.Index(line, "\"")+1 : strings.LastIndex(line, "\"")]
			if !containsString(guardrailPersonalDataRuleIDs, id) {
				t.Errorf("the gateway's rule %q is missing from guardrailPersonalDataRuleIDs", id)
			}
		}
	}
}
