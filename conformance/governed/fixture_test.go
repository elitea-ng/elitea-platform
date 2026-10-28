//go:build conformance

package governed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// world is one phase's view of the stack: the configuration and the four
// personas, each with its own PAT.
type world struct {
	cfg      config
	user     *persona // runs the governed turns (U)
	admin    *persona // proposes, stages and attaches (admin A)
	approver *persona // approves and activates (admin B)
	member   *persona // a project member without models.credentials.governed.use
}

func newWorld(ctx context.Context, t *testing.T, phase string) *world {
	t.Helper()
	cfg := loadConfig(t, phase)
	w := &world{
		cfg:      cfg,
		user:     cfg.persona(ctx, t, cfg.User),
		admin:    cfg.persona(ctx, t, cfg.Admin),
		approver: cfg.persona(ctx, t, cfg.Approver),
		member:   cfg.persona(ctx, t, cfg.Member),
	}
	if w.admin.UserID == w.approver.UserID {
		t.Fatal("the admin and the approver are the same account; every two-person step would be refused")
	}
	w.markSensitiveTools(ctx, t)
	return w
}

// markSensitiveTools makes Jira's add_comments a sensitive tool, so every
// governed add_comments call pauses on a HITL decision (GOV-006, ROLLBACK).
//
// It has to be the platform guardrails section, not the worker's
// --toolkit-security-config file: elitea-main sends this section with every
// turn (resolveToolkitGuardrails), and the worker treats a per-run policy as
// authoritative even when it is empty (policy_for_guardrails), so the file is
// only the fallback for a command that carries none. The save is a per-key
// upsert and idempotent, so every phase writes it again.
func (w *world) markSensitiveTools(ctx context.Context, t *testing.T) {
	t.Helper()
	w.admin.do(ctx, t, http.MethodPut, "/api/v2/admin/plugin_config_values/administration/guardrails",
		map[string]any{"values": map[string]any{"sensitive_tools": map[string]any{
			"*": []string{"delete_file", "remove_index"}, "jira": []string{"add_comments"},
		}}}, http.StatusOK)
}

func (w *world) p() string { return strconv.FormatInt(w.cfg.Project, 10) }

func idOf(t *testing.T, object map[string]any, key string) int64 {
	t.Helper()
	switch value := object[key].(type) {
	case float64:
		return int64(value)
	case string:
		id, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			return id
		}
	}
	t.Fatalf("no numeric %q in %v", key, object)
	return 0
}

func textOf(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, _ := object[key].(string)
	if value == "" {
		t.Fatalf("no %q in %v", key, object)
	}
	return value
}

// ── the platform objects every profile has ───────────────────────────────────

// jiraToolkit creates a sealed project secret holding token, a Jira
// configuration whose token is that secret, and a toolkit on it. This is the
// exact shape migrate-references turns into a static_secret reference.
func (w *world) jiraToolkit(ctx context.Context, t *testing.T, as *persona, label, token string, tools []string) int64 {
	t.Helper()
	secret := "gov_" + strings.ReplaceAll(label, "-", "_") + "_" + shortID()
	as.do(ctx, t, http.MethodPost, "/api/v2/secrets/secrets/default/"+w.p(),
		map[string]any{"name": secret, "value": token}, http.StatusCreated, http.StatusOK)
	title := "gov-" + label + "-" + shortID()
	as.do(ctx, t, http.MethodPost, "/api/v2/configurations/configurations/"+w.p(), map[string]any{
		"type": "jira", "elitea_title": title, "label": title, "shared": false,
		"data": map[string]any{
			"base_url": "https://jira.governed.test", "hosting": "server",
			"token": "{{secret." + secret + "}}",
		},
	}, http.StatusCreated, http.StatusOK)
	created := as.do(ctx, t, http.MethodPost, "/api/v2/elitea_core/tools/prompt_lib/"+w.p(), map[string]any{
		"name": title, "type": "jira", "description": "governed conformance Jira toolkit",
		"settings": map[string]any{
			"jira_configuration": map[string]any{"elitea_title": title, "private": false},
			"selected_tools":     tools,
		},
	}, http.StatusCreated).Object(t)
	return idOf(t, created, "id")
}

// agent creates an agent on the mock model and returns its id and version.
func (w *world) agent(ctx context.Context, t *testing.T, as *persona, label string) (int64, int64) {
	t.Helper()
	return w.application(ctx, t, as, label, map[string]any{
		"name": "base", "agent_type": "openai",
		"instructions": "You are the governed conformance agent. Call the tool the user names.",
		"llm_settings": map[string]any{"model_name": w.cfg.Model, "temperature": 0.1, "max_tokens": 512},
		"meta":         map[string]any{"step_limit": 25, "internal_tools": []string{}},
		"tools":        []any{}, "tags": []any{}, "variables": []any{}, "conversation_starters": []any{},
	})
}

// pipelineGraph is the smallest graph the runtime runs (one LLM node).
const pipelineGraph = `state:
  input:
    type: str
  messages:
    type: list
entry_point: LLM_1
nodes:
  - id: LLM_1
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are a helpful assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`

func (w *world) pipeline(ctx context.Context, t *testing.T, as *persona, label string) (int64, int64) {
	t.Helper()
	return w.application(ctx, t, as, label, map[string]any{
		"name": "base", "agent_type": "pipeline", "instructions": pipelineGraph,
		"llm_settings": map[string]any{"model_name": w.cfg.Model},
		"meta":         map[string]any{"step_limit": 25, "internal_tools": []string{}},
		"tools":        []any{}, "tags": []any{}, "variables": []any{}, "conversation_starters": []any{},
		"pipeline_settings": map[string]any{"nodes": []any{}, "edges": []any{}},
	})
}

func (w *world) application(ctx context.Context, t *testing.T, as *persona, label string, version map[string]any) (int64, int64) {
	t.Helper()
	name := "gov-" + label + "-" + shortID()
	created := as.do(ctx, t, http.MethodPost, "/api/v2/elitea_core/applications/prompt_lib/"+w.p(), map[string]any{
		"name": name, "description": "Governed-profile conformance fixture " + label,
		"type": "interface", "versions": []any{version},
	}, http.StatusCreated).Object(t)
	details, _ := created["version_details"].(map[string]any)
	if details == nil {
		t.Fatalf("the created application has no version_details: %v", created)
	}
	return idOf(t, created, "id"), idOf(t, details, "id")
}

// attachToolkit puts the toolkit on the agent version.
func (w *world) attachToolkit(ctx context.Context, t *testing.T, as *persona, toolkitID, applicationID, versionID int64) {
	t.Helper()
	as.do(ctx, t, http.MethodPatch, fmt.Sprintf("/api/v2/elitea_core/tool/prompt_lib/%s/%d", w.p(), toolkitID), map[string]any{
		"entity_version_id": versionID, "entity_id": applicationID, "entity_type": "agent", "has_relation": true,
	}, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

// ── governance setup, through the operator routes ────────────────────────────

// bindAgent gives the agent version an active binding: an elitea_native
// external identity created and provisioned by the admin, a binding proposed
// by the admin, approved by the approver, and the identity activated by the
// approver (activation needs the approved binding).
func (w *world) bindAgent(ctx context.Context, t *testing.T, applicationID, versionID int64) string {
	t.Helper()
	external := w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/external_agent_identities", map[string]any{
		"tenant_project_id": w.cfg.Project, "application_id": applicationID, "mode": "elitea_native",
		"provider": "elitea", "provider_tenant": "elitea.governed.test", "provider_config_key": "governed-conformance",
		"owner_ref": "governed-conformance",
	}, http.StatusCreated).Object(t)
	externalID := textOf(t, external, "id")
	w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/external_agent_identities/"+externalID+":provision", map[string]any{}, http.StatusOK)
	binding := w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/agent_identities", map[string]any{
		"tenant_project_id": w.cfg.Project, "application_id": applicationID, "application_version_id": versionID,
		"definition_digest": sha(fmt.Sprintf("governed-conformance/%d/%d", applicationID, versionID)),
		"mode":              "elitea_native", "external_agent_identity_id": externalID,
	}, http.StatusCreated).Object(t)
	bindingID := textOf(t, binding, "id")
	// The proposal named a placeholder digest; admission binds only the
	// canonical digest of the version (#1462), which the view computes.
	if binding["computed_definition_digest"] == nil || binding["computed_definition_digest"] == binding["definition_digest"] {
		t.Fatalf("the binding view computes no different digest: %v", binding)
	}
	w.repinAgent(ctx, t, bindingID)
	activated := w.approver.do(ctx, t, http.MethodPost, "/api/v2/admin/external_agent_identities/"+externalID+":activate", map[string]any{}, http.StatusOK).Object(t)
	if activated["state"] != "active" {
		t.Fatalf("the external identity did not activate: %v", activated)
	}
	return bindingID
}

// repinAgent moves the binding to the computed definition digest of its
// version (#1462) and has the approver re-approve it: the admin re-pins (a
// new proposal of the same binding), the approver approves. Attaching a
// reference rewrites the toolkit settings the agent definition embeds, so
// every attach is followed by a re-pin, as the runbook says.
func (w *world) repinAgent(ctx context.Context, t *testing.T, bindingID string) {
	t.Helper()
	path := "/api/v2/admin/agent_identities/" + bindingID
	repinned := w.admin.do(ctx, t, http.MethodPost, path+":repin", map[string]any{}, http.StatusOK).Object(t)
	if repinned["state"] != "proposed" || repinned["definition_digest"] != repinned["computed_definition_digest"] {
		t.Fatalf("a re-pinned binding is not a proposal of the computed digest: %v", repinned)
	}
	// The re-pinner is the proposer: it may not approve its own re-pin.
	w.admin.do(ctx, t, http.MethodPost, path+":approve", map[string]any{}, http.StatusForbidden)
	approved := w.approver.do(ctx, t, http.MethodPost, path+":approve", map[string]any{}, http.StatusOK).Object(t)
	if approved["state"] != "active" {
		t.Fatalf("an approved binding is not active: %v", approved)
	}
}

// migrationLine is one --json line of `credential-governance
// migrate-references`.
type migrationLine struct {
	Action                string `json:"action"`
	Reason                string `json:"reason"`
	ProjectID             int64  `json:"project_id"`
	ToolkitID             int64  `json:"toolkit_id"`
	ToolkitType           string `json:"toolkit_type"`
	ConfigurationUUID     string `json:"configuration_uuid"`
	ProviderType          string `json:"provider_type"`
	Class                 string `json:"class"`
	TargetIdentity        string `json:"target_identity"`
	TargetBindingID       string `json:"target_binding_id"`
	CredentialReferenceID string `json:"credential_reference_id"`
	Backend               string `json:"backend"`
	Locator               string `json:"locator"`
	Generation            int64  `json:"generation"`
	SecretLocation        string `json:"secret_location"`
	State                 string `json:"state"`
}

// migrateReferences runs the operator CLI the way the runbook says, as admin
// A, and returns the lines that concern toolkitID.
func (w *world) migrateReferences(ctx context.Context, t *testing.T, toolkitID int64, apply bool, extra ...string) []migrationLine {
	t.Helper()
	// --operator names the stager, so the CLI takes it only with --apply.
	args := []string{"credential-governance", "migrate-references", "--project", w.p(), "--json"}
	if apply {
		args = append(args, "--apply", "--operator", strconv.FormatInt(w.admin.UserID, 10))
	}
	args = append(args, extra...)
	out, stderr, err := w.cfg.mainCLI(ctx, args...)
	if err != nil {
		t.Fatalf("migrate-references %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, out, stderr)
	}
	var mine []migrationLine
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var parsed migrationLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("migrate-references printed a line that is not its JSON form: %q", line)
		}
		if parsed.ToolkitID == toolkitID {
			mine = append(mine, parsed)
		}
	}
	if len(mine) == 0 {
		t.Fatalf("migrate-references reported nothing for toolkit %d:\n%s", toolkitID, out)
	}
	return mine
}

// stageReference stages the toolkit's reference on backend ("pgvault" or
// "vault") and returns its line. A dry run first must propose and mint no id.
func (w *world) stageReference(ctx context.Context, t *testing.T, toolkitID int64, backend string) migrationLine {
	t.Helper()
	var extra []string
	if backend == "vault" {
		extra = []string{"--copy-to", "vault"}
	}
	// The dry run writes nothing: same rows before and after (#1124
	// acceptance: "--dry-run makes no writes").
	const rows = `SELECT (SELECT count(*) FROM elitea_identity.credential_references) || ' ' ||
       (SELECT count(*) FROM elitea_identity.target_bindings);`
	before := w.cfg.psqlOne(ctx, t, rows, nil)
	for _, line := range w.migrateReferences(ctx, t, toolkitID, false, extra...) {
		if line.Action != "propose" || line.CredentialReferenceID != "" {
			t.Fatalf("a dry run must only propose and mint no id: %+v", line)
		}
	}
	if after := w.cfg.psqlOne(ctx, t, rows, nil); after != before {
		t.Fatalf("a dry run wrote rows: references/targets %s -> %s", before, after)
	}
	for _, line := range w.migrateReferences(ctx, t, toolkitID, true, extra...) {
		if line.Backend != backend {
			continue
		}
		if line.Action != "staged" || line.CredentialReferenceID == "" || line.Class != "static_secret" || line.Generation != 1 {
			t.Fatalf("migrate-references did not stage a static_secret reference on %s: %+v", backend, line)
		}
		return line
	}
	t.Fatalf("migrate-references staged nothing on backend %s for toolkit %d", backend, toolkitID)
	return migrationLine{}
}

const referencesPath = "/api/v2/admin/credential_references/"

// activateReference is the second person's step: the approver activates
// what the admin staged.
func (w *world) activateReference(ctx context.Context, t *testing.T, referenceID string) {
	t.Helper()
	// The stager may not activate its own reference.
	w.admin.do(ctx, t, http.MethodPost, referencesPath+referenceID+":activate", map[string]any{}, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity)
	active := w.approver.do(ctx, t, http.MethodPost, referencesPath+referenceID+":activate", map[string]any{}, http.StatusOK).Object(t)
	if active["state"] != "active" {
		t.Fatalf("an activated reference is not active: %v", active)
	}
}

func (w *world) attachReference(ctx context.Context, t *testing.T, referenceID string, toolkitID int64) {
	t.Helper()
	w.admin.do(ctx, t, http.MethodPost, referencesPath+referenceID+":attach",
		map[string]any{"project_id": w.cfg.Project, "toolkit_id": toolkitID}, http.StatusOK)
}

// bindToolkit pins the toolkit's CURRENT digest to the binding, server side.
// This is the one place the suite writes an agent toolkit binding.
func (w *world) bindToolkit(ctx context.Context, t *testing.T, bindingID string, toolkitID int64) {
	t.Helper()
	w.admin.do(ctx, t, http.MethodPut, fmt.Sprintf("/api/v2/admin/agent_identities/%s/toolkits/%d", bindingID, toolkitID),
		map[string]any{"project_id": w.cfg.Project}, http.StatusOK, http.StatusCreated)
}

// grant proposes (admin) and approves (approver) one credential use grant.
func (w *world) grant(ctx context.Context, t *testing.T, bindingID string, toolkitID int64, reference migrationLine, capabilities, operations []string) string {
	t.Helper()
	proposed := w.admin.do(ctx, t, http.MethodPost, "/api/v2/projects/"+w.p()+"/credential_grants", map[string]any{
		"agent_binding_id": bindingID, "toolkit_id": toolkitID,
		"credential_reference_id": reference.CredentialReferenceID, "target_binding_id": reference.TargetBindingID,
		"capabilities": capabilities, "operation_scopes": operations, "resource_scopes": []string{}, "obligations": []string{},
	}, http.StatusCreated).Object(t)
	id := textOf(t, proposed, "id")
	// The proposer may not approve its own grant.
	w.admin.do(ctx, t, http.MethodPost, "/api/v2/projects/"+w.p()+"/credential_grants/"+id+":approve", map[string]any{},
		http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity)
	approved := w.approver.do(ctx, t, http.MethodPost, "/api/v2/projects/"+w.p()+"/credential_grants/"+id+":approve", map[string]any{}, http.StatusOK).Object(t)
	if approved["state"] != "active" {
		t.Fatalf("an approved grant is not active: %v", approved)
	}
	return id
}

// mandate issues (admin) and approves (approver) one mandate.
func (w *world) mandate(ctx context.Context, t *testing.T, kind, bindingID string, subject *persona, operations, resources []string) string {
	t.Helper()
	body := map[string]any{
		"tenant_project_id": w.cfg.Project, "kind": kind, "agent_binding_id": bindingID,
		"operation_scopes": operations, "resource_scopes": resources,
		"expires_at": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	}
	if subject != nil {
		body["subject_user_id"] = subject.UserID
	}
	issued := w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/mandates", body, http.StatusCreated).Object(t)
	id := textOf(t, issued, "id")
	approved := w.approver.do(ctx, t, http.MethodPost, "/api/v2/admin/mandates/"+id+":approve", map[string]any{}, http.StatusOK).Object(t)
	if approved["state"] != "active" {
		t.Fatalf("an approved mandate is not active: %v", approved)
	}
	return id
}

// ── a governed agent, end to end ─────────────────────────────────────────────

var jiraOperations = []string{"jira.project.read", "jira.issue.write"}

type agentSpec struct {
	Label string
	// GrantOperations default to every Jira operation the cases call.
	GrantOperations []string
	// MandateFor issues a delegated_background mandate naming that persona;
	// nil issues none (interactive authority then has nothing to stand on).
	MandateFor []*persona
	// BindBeforeAttach writes the toolkit binding before the reference is
	// attached, so the pinned digest is the pre-attach one.
	BindBeforeAttach bool
	// Backend is where the reference lives: pgvault (default) or vault.
	Backend string
}

type governedAgent struct {
	turnFixture
	Token      string
	BindingID  string
	Reference  migrationLine
	GrantID    string
	MandateIDs []string
}

// governedAgent builds one isolated governed agent: its own secret (a
// canary), configuration, toolkit, agent, binding, reference, grant and
// mandates. Cases never share one, so a revocation in one case cannot make
// another pass or fail.
func (w *world) governedAgent(ctx context.Context, t *testing.T, spec agentSpec) governedAgent {
	t.Helper()
	if spec.Backend == "" {
		spec.Backend = "pgvault"
	}
	if spec.GrantOperations == nil {
		spec.GrantOperations = jiraOperations
	}
	token := w.cfg.newCanary(t, spec.Label)
	toolkitID := w.jiraToolkit(ctx, t, w.admin, spec.Label, token, []string{"list_projects", "add_comments"})
	applicationID, versionID := w.agent(ctx, t, w.admin, spec.Label)
	w.attachToolkit(ctx, t, w.admin, toolkitID, applicationID, versionID)
	bindingID := w.bindAgent(ctx, t, applicationID, versionID)
	reference := w.stageReference(ctx, t, toolkitID, spec.Backend)
	w.activateReference(ctx, t, reference.CredentialReferenceID)
	if spec.BindBeforeAttach {
		w.bindToolkit(ctx, t, bindingID, toolkitID)
		w.attachReference(ctx, t, reference.CredentialReferenceID, toolkitID)
		// The agent's definition digest moved with the attach; re-pin it,
		// but not the toolkit's, so only the toolkit gate can deny.
		w.repinAgent(ctx, t, bindingID)
	} else {
		w.attachReference(ctx, t, reference.CredentialReferenceID, toolkitID)
		w.repinAgent(ctx, t, bindingID)
		w.bindToolkit(ctx, t, bindingID, toolkitID)
	}
	grantID := w.grant(ctx, t, bindingID, toolkitID, reference, []string{"use", "probe"}, spec.GrantOperations)
	agent := governedAgent{
		turnFixture: turnFixture{ApplicationID: applicationID, VersionID: versionID, ToolkitID: toolkitID,
			CredentialSHA256: sha(token)},
		Token: token, BindingID: bindingID, Reference: reference, GrantID: grantID,
	}
	for _, subject := range spec.MandateFor {
		agent.MandateIDs = append(agent.MandateIDs,
			w.mandate(ctx, t, "delegated_background", bindingID, subject, jiraOperations, []string{}))
	}
	return agent
}

// ── conversations and turns ──────────────────────────────────────────────────

const (
	executeContract  = "agent.execute.application.v1"
	continueContract = "agent.continue.hitl.v1"
)

// converse opens a conversation for as with the agent as a participant and
// returns the fixture carrying it.
func (w *world) converse(ctx context.Context, t *testing.T, as *persona, fixture turnFixture) turnFixture {
	t.Helper()
	created := as.do(ctx, t, http.MethodPost, "/api/v2/elitea_core/conversations/prompt_lib/"+w.p(),
		map[string]any{"name": "governed conformance " + shortID()}, http.StatusCreated).Object(t)
	fixture.ConversationID = strconv.FormatInt(idOf(t, created, "id"), 10)
	fixture.ConversationUUID = textOf(t, created, "uuid")
	as.do(ctx, t, http.MethodPost, "/api/v2/elitea_core/participants/prompt_lib/"+w.p()+"/"+fixture.ConversationID, []any{
		map[string]any{"entity_name": "application",
			"entity_meta":     map[string]any{"id": fixture.ApplicationID, "project_id": w.cfg.Project},
			"entity_settings": map[string]any{"version_id": fixture.VersionID}},
		map[string]any{"entity_name": "user", "entity_meta": map[string]any{"id": as.UserID}},
	}, http.StatusOK, http.StatusCreated)
	conversation := as.do(ctx, t, http.MethodGet, "/api/v2/elitea_core/conversation/prompt_lib/"+w.p()+"/"+fixture.ConversationID,
		nil, http.StatusOK).Object(t)
	participants, _ := conversation["participants"].([]any)
	for _, raw := range participants {
		participant, _ := raw.(map[string]any)
		meta, _ := participant["entity_meta"].(map[string]any)
		if participant["entity_name"] == "application" && meta != nil && idOf(t, meta, "id") == fixture.ApplicationID {
			fixture.ParticipantID = idOf(t, participant, "id")
			return fixture
		}
	}
	t.Fatalf("the conversation has no participant for application %d: %v", fixture.ApplicationID, participants)
	return fixture
}

// sendResult is the admission answer to one turn.
type sendResult struct {
	Status            int
	RefusalCode       string
	ExecutionID       string
	ResponseMessageID string
	Raw               response
}

func (w *world) send(ctx context.Context, t *testing.T, as *persona, fixture turnFixture, prompt string) sendResult {
	t.Helper()
	answer := as.raw(ctx, t, http.MethodPost,
		"/api/v2/elitea_core/messages/prompt_lib/"+w.p()+"/"+fixture.ConversationUUID+"?execution_contract="+executeContract,
		map[string]any{
			"project_id": w.cfg.Project, "conversation_uuid": fixture.ConversationUUID,
			"participant_id": fixture.ParticipantID, "question_id": newUUID(), "interaction_uuid": newUUID(),
			"payload": map[string]any{"user_input": prompt},
		}, nil)
	result := sendResult{Status: answer.Status, Raw: answer}
	switch answer.Status {
	case http.StatusOK:
		var admitted struct {
			ExecutionID       string `json:"execution_id"`
			ResponseMessageID string `json:"response_message_id"`
		}
		answer.JSON(t, &admitted)
		if admitted.ExecutionID == "" || admitted.ResponseMessageID == "" {
			t.Fatalf("an admitted turn names no execution: %s", answer)
		}
		result.ExecutionID, result.ResponseMessageID = admitted.ExecutionID, admitted.ResponseMessageID
	case http.StatusForbidden:
		body := answer.Object(t)
		if body["error"] != "governed_credential_refused" {
			t.Fatalf("a 403 that is not a governed refusal: %s", answer)
		}
		result.RefusalCode, _ = body["code"].(string)
	default:
		t.Fatalf("a turn answered neither admitted nor refused: %s", answer)
	}
	return result
}

func (w *world) mustAdmit(ctx context.Context, t *testing.T, as *persona, fixture turnFixture, prompt string) sendResult {
	t.Helper()
	result := w.send(ctx, t, as, fixture, prompt)
	if result.Status != http.StatusOK {
		t.Fatalf("the turn was refused (%s): %s", result.RefusalCode, result.Raw)
	}
	return result
}

// messages is the conversation's transcript, each message as raw JSON.
func (w *world) messages(ctx context.Context, t *testing.T, as *persona, fixture turnFixture) []map[string]any {
	t.Helper()
	answer := as.do(ctx, t, http.MethodGet, "/api/v2/elitea_core/messages/prompt_lib/"+w.p()+"/"+fixture.ConversationID+
		"?sort_order=asc&limit=100", nil, http.StatusOK).Object(t)
	list, ok := answer["items"].([]any)
	if !ok {
		list, ok = answer["rows"].([]any)
	}
	if !ok {
		t.Fatalf("the transcript has neither items nor rows: %v", answer)
	}
	out := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		if message, ok := raw.(map[string]any); ok {
			out = append(out, message)
		}
	}
	return out
}

func messageRaw(message map[string]any) string {
	raw, _ := json.Marshal(message)
	return string(raw)
}

func metadataOf(message map[string]any) map[string]any {
	meta, _ := message["metadata"].(map[string]any)
	return meta
}

// settled waits until no message of the conversation is streaming and the
// newest one is not waiting on a person, and returns the newest message.
func (w *world) settled(ctx context.Context, t *testing.T, as *persona, fixture turnFixture, timeout time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last map[string]any
	var seen []map[string]any
	// eventually fails through t.Fatal, which still runs this: say what the
	// turn looked like, not only that it never settled.
	defer func() {
		if t.Failed() {
			t.Logf("last messages seen: %s", summarizeMessages(seen))
		}
	}()
	eventually(ctx, t, 2*time.Second, "the turn to settle", func() bool {
		list := w.messages(ctx, t, as, fixture)
		seen = list
		if len(list) == 0 {
			return false
		}
		for _, message := range list {
			if streaming, _ := message["is_streaming"].(bool); streaming {
				return false
			}
		}
		last = list[len(list)-1]
		return metadataOf(last)["hitl_interrupt"] == nil
	})
	return last
}

// summarizeMessages renders the fields that say where a turn stopped:
// streaming, the HITL state and the message text, cut short.
func summarizeMessages(list []map[string]any) string {
	var out []string
	for _, message := range list {
		raw := messageRaw(message)
		if len(raw) > 300 {
			raw = raw[:300] + "…"
		}
		out = append(out, fmt.Sprintf("{id=%v role=%v streaming=%v hitl=%v meta_keys=%v text=%q}",
			message["id"], message["role"], message["is_streaming"], metadataOf(message)["hitl_interrupt"] != nil,
			slices.Sorted(maps.Keys(metadataOf(message))), raw))
	}
	return "[" + strings.Join(out, " ") + "]"
}

// interrupt waits for the turn to pause on a HITL decision and returns it.
func (w *world) interrupt(ctx context.Context, t *testing.T, as *persona, fixture turnFixture, messageID string) pausedRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	var paused pausedRun
	var seen []map[string]any
	defer func() {
		if t.Failed() {
			t.Logf("last messages seen: %s", summarizeMessages(seen))
		}
	}()
	eventually(ctx, t, 2*time.Second, "the run to pause on a HITL decision", func() bool {
		seen = w.messages(ctx, t, as, fixture)
		for _, message := range seen {
			pending, _ := metadataOf(message)["hitl_interrupt"].(map[string]any)
			if pending == nil {
				continue
			}
			paused = pausedRun{Turn: fixture, MessageID: messageID}
			paused.InterruptID, _ = pending["interrupt_id"].(string)
			paused.ToolCallID, _ = pending["tool_call_id"].(string)
			actions, _ := pending["available_actions"].([]any)
			for _, action := range actions {
				if name, _ := action.(string); strings.Contains(name, "approve") {
					paused.ApproveAction = name
				}
			}
			if paused.InterruptID == "" || paused.ApproveAction == "" {
				t.Fatalf("a HITL pause with no interrupt id or no approve action: %s", messageRaw(message))
			}
			return true
		}
		return false
	})
	return paused
}

// resume answers a paused run with its approve action.
func (w *world) resume(ctx context.Context, t *testing.T, as *persona, paused pausedRun) response {
	t.Helper()
	return as.raw(ctx, t, http.MethodPost,
		"/api/v2/elitea_core/continue_predict/prompt_lib/"+w.p()+"/"+paused.Turn.ConversationUUID+"?execution_contract="+continueContract,
		map[string]any{
			"project_id": w.cfg.Project, "conversation_uuid": paused.Turn.ConversationUUID,
			"message_id": paused.MessageID, "hitl_resume": true,
			"hitl_decisions": []any{map[string]any{
				"interrupt_id": paused.InterruptID, "tool_call_id": paused.ToolCallID,
				"action": paused.ApproveAction, "value": nil,
			}},
		}, nil)
}

// ── what the stack recorded ──────────────────────────────────────────────────

func (w *world) admissionProfile(ctx context.Context, t *testing.T, executionID string) string {
	t.Helper()
	rows := w.cfg.psql(ctx, t, `SELECT profile FROM elitea_runtime.execution_admission_profiles WHERE execution_id = :'id';`,
		map[string]string{"id": executionID})
	if len(rows) != 1 {
		t.Fatalf("execution %s has %d admission profile rows", executionID, len(rows))
	}
	return rows[0]
}

const governedProfile = "elitea.credential-profile.governed.v1"

// decision is one security decision event the outbox holds for a run.
type decision struct {
	Effect  string
	Reasons []string
}

func (w *world) decisions(ctx context.Context, t *testing.T, executionID string) []decision {
	t.Helper()
	rows := w.cfg.psql(ctx, t, `
SELECT coalesce(event->'data'->>'effect', '') || '|' ||
       coalesce((SELECT string_agg(r, ',') FROM jsonb_array_elements_text(event->'data'->'reasons') AS r), '')
  FROM elitea_runtime.security_audit_outbox
 WHERE event_type = 'ai.elitea.security.v1.decision'
   AND event->'data'->>'execution_id' = :'id'
 ORDER BY seq;`, map[string]string{"id": executionID})
	out := make([]decision, 0, len(rows))
	for _, row := range rows {
		effect, reasons, _ := strings.Cut(row, "|")
		d := decision{Effect: effect}
		if reasons != "" {
			d.Reasons = strings.Split(strings.ToLower(reasons), ",")
		}
		out = append(out, d)
	}
	return out
}

func (w *world) redemptions(ctx context.Context, t *testing.T, executionID string) int64 {
	t.Helper()
	return w.cfg.psqlInt(ctx, t, `SELECT count(*) FROM elitea_runtime.credential_redemptions WHERE execution_id = :'id';`,
		map[string]string{"id": executionID})
}

// expectDenied asserts one governed refusal: either the admission refused
// the turn with one of sendCodes, or it admitted it and a decision denied
// the credential with one of reasons. Either way the provider never saw the
// credential and nothing was redeemed.
func (w *world) expectDenied(ctx context.Context, t *testing.T, as *persona, agent governedAgent, result sendResult, sendCodes, reasons []string) {
	t.Helper()
	before := len(w.cfg.callsWith(ctx, t, "jira.governed.test", agent.Token))
	switch result.Status {
	case http.StatusForbidden:
		if !slices.Contains(sendCodes, result.RefusalCode) {
			t.Fatalf("refused at admission with %q, want one of %v", result.RefusalCode, sendCodes)
		}
	case http.StatusOK:
		w.settled(ctx, t, as, agent.turnFixture, 4*time.Minute)
		var denied bool
		decisions := w.decisions(ctx, t, result.ExecutionID)
		// A run whose admission froze no reference (its binding was not
		// usable) is refused by the broker before any decision is made.
		if slices.Contains(reasons, unfrozenRefusal) && len(decisions) == 0 {
			denied = true
		}
		for _, d := range decisions {
			if strings.EqualFold(d.Effect, "deny") {
				for _, reason := range d.Reasons {
					if slices.Contains(reasons, reason) {
						denied = true
					}
				}
			}
		}
		if !denied {
			t.Fatalf("execution %s: no deny decision with a reason in %v (decisions %+v)", result.ExecutionID, reasons,
				w.decisions(ctx, t, result.ExecutionID))
		}
		if n := w.redemptions(ctx, t, result.ExecutionID); n != 0 {
			t.Fatalf("execution %s: %d credentials were redeemed by a denied run", result.ExecutionID, n)
		}
	}
	if after := len(w.cfg.callsWith(ctx, t, "jira.governed.test", agent.Token)); after != before {
		t.Fatalf("the provider saw the credential %d more times after a denial", after-before)
	}
}

// unfrozenRefusal, in expectDenied's reasons, also accepts a run with no
// decision at all: the broker refuses credential_reference_not_frozen first.
const unfrozenRefusal = "credential_reference_not_frozen"

// expectUsed asserts a turn used the governed credential: admitted on the
// governed profile, the provider saw exactly the reference's secret, and a
// security context, a bound invocation and a redemption were recorded.
func (w *world) expectUsed(ctx context.Context, t *testing.T, as *persona, agent governedAgent, fixture turnFixture, prompt, sentinel string) sendResult {
	t.Helper()
	before := len(w.cfg.callsWith(ctx, t, "jira.governed.test", agent.Token))
	result := w.mustAdmit(ctx, t, as, fixture, prompt)
	last := w.settled(ctx, t, as, fixture, 4*time.Minute)
	if !strings.Contains(messageRaw(last), sentinel) {
		t.Fatalf("the answer does not carry the provider's sentinel %s: %s", sentinel, messageRaw(last))
	}
	if after := len(w.cfg.callsWith(ctx, t, "jira.governed.test", agent.Token)); after <= before {
		t.Fatal("the provider never saw the reference's secret")
	}
	if profile := w.admissionProfile(ctx, t, result.ExecutionID); profile != governedProfile {
		t.Fatalf("execution %s was pinned to %s", result.ExecutionID, profile)
	}
	vars := map[string]string{"id": result.ExecutionID, "ref": agent.Reference.CredentialReferenceID}
	if n := w.cfg.psqlInt(ctx, t, `SELECT count(*) FROM elitea_runtime.execution_security_contexts WHERE execution_id = :'id';`, vars); n != 1 {
		t.Fatalf("execution %s has %d security contexts", result.ExecutionID, n)
	}
	if n := w.cfg.psqlInt(ctx, t, `SELECT count(*) FROM elitea_runtime.bound_tool_invocations
 WHERE execution_id = :'id' AND credential_reference_id = :'ref'::uuid;`, vars); n == 0 {
		t.Fatalf("execution %s bound no invocation to reference %s", result.ExecutionID, agent.Reference.CredentialReferenceID)
	}
	if n := w.redemptions(ctx, t, result.ExecutionID); n == 0 {
		t.Fatalf("execution %s redeemed nothing", result.ExecutionID)
	}
	return result
}

func callTool(tool, arguments string) string {
	if arguments == "" {
		return "[[mock:call_tool " + tool + "]]"
	}
	return "[[mock:call_tool " + tool + " " + arguments + "]]"
}
