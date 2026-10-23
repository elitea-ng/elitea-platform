package agentexecution

// The guardrails policy reaching the agent execution input.
//
// The freeze (tools_guardrails_test.go) removes what a saved agent NAMES. This
// is the other half: the sensitive-tool policy, which the freeze cannot enforce
// because "ask the user first" is a decision made at tool-call time inside the
// worker. If the field does not reach the wire, the worker falls back to its
// container environment and the admin page's setting does nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// The same target every other start test uses, minimal for this file's purpose:
// these cases assert what lands on the WIRE, not how a version is frozen.
func currentApplicationResolverForStamping() *currentApplicationResolverStub {
	return &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Be concise","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
}

func guardrailPolicyForTest() guardrails.Policy {
	return guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"Shell"},
		BlockedTools:    map[string][]string{"GitHub": {"Create-Issue"}},
		SensitiveTools:  map[string][]string{"*": {"delete_file"}},
		CompanyName:     "Acme",
		MessageTemplate: "{company_name} requires approval.",
	})
}

func decodeStampedGuardrails(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("no toolkit guardrails were stamped onto the execution input")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("stamped guardrails are not an object: %s", raw)
	}
	return decoded
}

func TestTheStartInputCarriesTheGuardrailsPolicy(t *testing.T) {
	admissions := &currentApplicationAdmissionStub{}
	resolver := currentApplicationResolverForStamping()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{policy: guardrailPolicyForTest()},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatal(err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("admissions=%d", len(admissions.requests))
	}

	stamped := decodeStampedGuardrails(t, admissions.requests[0].Input.ToolkitGuardrails)

	// CANONICAL keys, so the worker and this service cannot normalise
	// differently — the worker canonicalises whatever it receives, and
	// canonicalising a canonical key is a no-op.
	sensitive, ok := stamped["sensitive_tools"].(map[string]any)
	if !ok {
		t.Fatalf("sensitive_tools=%v", stamped["sensitive_tools"])
	}
	if _, present := sensitive["*"]; !present {
		t.Fatalf("the wildcard key did not survive: %v", sensitive)
	}
	blocked, ok := stamped["blocked_tools"].(map[string]any)
	if !ok || blocked["github"] == nil {
		t.Fatalf("blocked_tools=%v, want the canonical toolkit key", stamped["blocked_tools"])
	}
	// Display copy is carried RAW — canonicalising it would strip the
	// placeholder braces the template is made of.
	if stamped["sensitive_action_message_template"] != "{company_name} requires approval." {
		t.Fatalf("message template=%v", stamped["sensitive_action_message_template"])
	}
	if stamped["sensitive_action_company_name"] != "Acme" {
		t.Fatalf("company name=%v", stamped["sensitive_action_company_name"])
	}
}

func TestAnEmptyPolicyIsStampedRatherThanOmitted(t *testing.T) {
	// The worker treats an ABSENT field as "leave the container environment
	// alone" and an EMPTY object as "the platform resolved a policy and it is
	// empty". Omitting the field for an unconfigured platform would silently
	// hand control back to ELITEA_SENSITIVE_TOOLS, which is the behaviour this
	// unit exists to replace.
	admissions := &currentApplicationAdmissionStub{}
	resolver := currentApplicationResolverForStamping()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatal(err)
	}
	stamped := decodeStampedGuardrails(t, admissions.requests[0].Input.ToolkitGuardrails)
	if len(stamped["blocked_toolkits"].([]any)) != 0 {
		t.Fatalf("blocked_toolkits=%v", stamped["blocked_toolkits"])
	}
	// The SDK's own defaults, resolved here so one side owns the dialog copy.
	if stamped["sensitive_action_company_name"] != guardrails.DefaultSensitiveActionCompanyName {
		t.Fatalf("company name=%v", stamped["sensitive_action_company_name"])
	}
}

func TestAnUnreadablePolicyRefusesTheTurn(t *testing.T) {
	// The opposite of the suggestion policy beside it, which degrades to null.
	// A guardrails policy's degraded value is "no tool is sensitive", which runs
	// unprompted exactly the actions an operator marked as needing approval.
	admissions := &currentApplicationAdmissionStub{}
	resolver := currentApplicationResolverForStamping()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{err: errors.New("pool is gone")},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err == nil {
		t.Fatal("an unreadable guardrails policy must refuse the turn")
	}
	if len(admissions.requests) != 0 {
		t.Fatalf("a refused turn must not be admitted: %d", len(admissions.requests))
	}
}
