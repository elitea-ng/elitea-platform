package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0207 (ADR-0044 decision 1, issue #1501): the default-deny
// connector catalogue. CI-only: skipped without the integration database.
// The seed's project 1 is the requesting project; user 11 requests and
// user 12 approves.

const (
	connectorRequester int64 = 11
	connectorApprover  int64 = 12
)

func newConnectorTestStore(t *testing.T, pool *pgxpool.Pool, governed bool) (*connectorapproval.Gate, *ConnectorRequests) {
	t.Helper()
	// centry.platform_config comes from the bootstrap schema
	// (internal/infra/db/migrations/001_initial.sql), not the ledgered corpus.
	if _, err := pool.Exec(t.Context(), `
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key))`); err != nil {
		t.Fatal(err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewConnectorRequests(pool, appender, func(ctx context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.LoadCatalogueSettings(ctx, pool)
	})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := connectorapproval.NewGate(store.settings, store, governed)
	if err != nil {
		t.Fatal(err)
	}
	return gate, store
}

func setCatalogueSetting(t *testing.T, pool *pgxpool.Pool, key string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	newConnectorTestStore(t, pool, false) // creates the bootstrap table
	if _, err := pool.Exec(t.Context(), `
INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC', 'test')
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`,
		platformconfig.SectionCatalogue, key, string(encoded)); err != nil {
		t.Fatal(err)
	}
}

func connectorAuditActions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event->'data'->>'action' FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 ORDER BY seq`, string(securityaudit.TypeConnectorApproval))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	return actions
}

func requireConnectorRefusal(t *testing.T, err error, code string) {
	t.Helper()
	refusal, ok := connectorapproval.AsRefusal(err)
	if !ok || refusal.Code != code {
		t.Fatalf("got %v, want the refusal %s", err, code)
	}
}

// connectorInputs serves one input bundle entry to the command gate.
type connectorInputs map[string][]byte

func (inputs connectorInputs) InputEntries(_ context.Context, _ string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, id := range ids {
		if content, ok := inputs[id]; ok {
			out[id] = content
		}
	}
	return out, nil
}

// passEgress is the wrapped egress resolver: it answers an empty policy.
type passEgress struct{ calls int }

func (p *passEgress) WorkerEgressPolicy(context.Context, *runtimev1.WorkerCommandV1) (*runtimev1.WorkerEgressPolicyV1, error) {
	p.calls++
	return &runtimev1.WorkerEgressPolicyV1{}, nil
}

// agentCommand is an agent run of project 1 whose frozen toolkit set is
// tools.
func agentCommand(t *testing.T, tools []map[string]any, governed bool) (*runtimev1.WorkerCommandV1, connectorInputs) {
	t.Helper()
	encoded, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Tools: encoded})
	if err != nil {
		t.Fatal(err)
	}
	command := &runtimev1.WorkerCommandV1{
		ResourceProjectId: "1",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "request"},
		},
	}
	if governed {
		command.SecurityContextRef = &runtimev1.ExecutionSecurityContextReferenceV1{ContentEntryId: "security"}
	}
	return command, connectorInputs{"request": input}
}

// toolCallCommand is a direct call of one tool of a custom MCP toolkit.
func toolCallCommand(t *testing.T, url, tool string) (*runtimev1.WorkerCommandV1, connectorInputs) {
	t.Helper()
	settings, err := json.Marshal(map[string]any{"url": url})
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.WorkerCommandV1{
		ResourceProjectId: "1",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
			ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{ToolkitType: "mcp", SettingsEntryId: "settings", ToolName: tool},
		},
	}, connectorInputs{"settings": settings}
}

func admit(t *testing.T, gate *connectorapproval.Gate, command *runtimev1.WorkerCommandV1, inputs connectorInputs) error {
	t.Helper()
	next := &passEgress{}
	commandGate, err := connectorapproval.NewCommandGate(gate, inputs, next)
	if err != nil {
		t.Fatal(err)
	}
	_, err = commandGate.WorkerEgressPolicy(t.Context(), command)
	if err == nil && next.calls != 1 {
		t.Fatalf("an admitted command did not reach the egress resolver")
	}
	return err
}

func requireNotApproved(t *testing.T, err error) {
	t.Helper()
	var refusal *connectorapproval.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("got %v, want a CONNECTOR_NOT_APPROVED refusal", err)
	}
	if code, _ := refusal.DispatchRefusal(); code != connectorapproval.RefusalCode {
		t.Fatalf("refusal code %s", code)
	}
}

func approvedToolkitRequest(t *testing.T, store *ConnectorRequests, toolkitType string, hosts ...string) connectorapproval.Request {
	t.Helper()
	request, err := store.Create(t.Context(), 1, connectorRequester, connectorapproval.Draft{
		TargetKind: connectorapproval.TargetToolkitType, Target: toolkitType,
		Tools:         []connectorapproval.DeclaredTool{{Name: "search_issues", Category: "read"}},
		EgressHosts:   hosts,
		DataClasses:   []string{"internal"},
		Justification: "the support team triages issues",
	}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if request.State != connectorapproval.StateSubmitted || !slices.Equal(request.ProjectIDs, []int64{1}) {
		t.Fatalf("created %+v", request)
	}
	return request
}

// TestConnectorCatalogue_OpenModeIsUnchanged: with no setting (the default,
// open) nothing is gated: any type is offered and saved, and a run naming
// any toolkit is admitted, with no approved request at all.
func TestConnectorCatalogue_OpenModeIsUnchanged(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	gate, _ := newConnectorTestStore(t, pool, false)
	view, err := gate.ForProject(t.Context(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.ApprovedOnly() {
		t.Fatalf("the default mode is %s", view.Mode)
	}
	for _, toolkitType := range []string{"jira", "mcp_github", "mcp"} {
		if !view.AllowsType(toolkitType) || !view.Decide(toolkitType, map[string]any{"url": "https://mcp.example.com/mcp"}).Allowed {
			t.Fatalf("open mode refused %s", toolkitType)
		}
	}
	command, inputs := agentCommand(t, []map[string]any{{"type": "jira", "settings": map[string]any{}}}, false)
	if err := admit(t, gate, command, inputs); err != nil {
		t.Fatalf("open mode refused a run: %v", err)
	}
}

// TestConnectorCatalogue_ApprovedOnlyRefusesAnUnapprovedType: in
// approved_only an unapproved type is refused when a toolkit is saved (the
// decision the create, update and fork handlers make) and when a run that
// names it is admitted; once approved, both pass.
func TestConnectorCatalogue_ApprovedOnlyRefusesAnUnapprovedType(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	setCatalogueSetting(t, pool, platformconfig.KeyCatalogueMode, "approved_only")
	gate, store := newConnectorTestStore(t, pool, false)

	view, err := gate.ForProject(t.Context(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	decision := view.Decide("jira", nil)
	if decision.Allowed || decision.Kind != connectorapproval.TargetToolkitType || decision.Target != "jira" {
		t.Fatalf("create: unapproved type decided %+v", decision)
	}
	if view.AllowsType("jira") || !view.AllowsType("artifact") {
		t.Fatal("the type catalogue offers an unapproved type, or hides a platform-internal one")
	}
	command, inputs := agentCommand(t, []map[string]any{
		{"type": "jira", "settings": map[string]any{}},
		{"type": "application", "settings": map[string]any{}, "meta": map[string]any{"internal_builder": true}},
	}, false)
	requireNotApproved(t, admit(t, gate, command, inputs))

	request := approvedToolkitRequest(t, store, "jira", "jira.example.com")
	if _, err := store.Approve(t.Context(), request.ID, connectorApprover, "fine"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	view, err = gate.ForProject(t.Context(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Decide("jira", nil).Allowed || !view.AllowsType("jira") {
		t.Fatal("an approved type is still refused")
	}
	if view.Decide("confluence", nil).Allowed {
		t.Fatal("approving jira approved another type")
	}
	if err := admit(t, gate, command, inputs); err != nil {
		t.Fatalf("an approved run is refused: %v", err)
	}
	// Another project is not granted by project 1's approval.
	other, err := gate.ForProject(t.Context(), 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if other.Decide("jira", nil).Allowed {
		t.Fatal("a project approval leaked to another project")
	}
}

// TestConnectorCatalogue_ApprovalWritesGrantsAndEgressAtomically: one
// approval writes the toolkit type policy and grant, the project's egress
// allowlist entries, the state and the audit event; an approval that fails
// part way (the allowlist is full) writes none of them.
func TestConnectorCatalogue_ApprovalWritesGrantsAndEgressAtomically(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)

	request := approvedToolkitRequest(t, store, "jira", "jira.example.com", "api.atlassian.com:443")
	outcome, err := store.Approve(t.Context(), request.ID, connectorApprover, "reviewed")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if outcome.Request.State != connectorapproval.StateApproved || outcome.Request.DecidedBy != connectorApprover ||
		!slices.Equal(outcome.GrantedProjects, []int64{1}) {
		t.Fatalf("outcome %+v", outcome)
	}
	var policy, grant string
	if err := pool.QueryRow(t.Context(), `SELECT availability FROM centry.toolkit_type_policy WHERE toolkit_type = 'jira'`).Scan(&policy); err != nil {
		t.Fatalf("policy row: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `
SELECT availability FROM centry.toolkit_type_project_grant WHERE toolkit_type = 'jira' AND project_id = 1`).Scan(&grant); err != nil {
		t.Fatalf("grant row: %v", err)
	}
	if policy != "enabled" || grant != "enabled" {
		t.Fatalf("policy %s grant %s", policy, grant)
	}
	allowlist := projectAllowlist(t, pool)
	for _, host := range []string{"jira.example.com", "api.atlassian.com:443"} {
		if !slices.Contains(allowlist, host) {
			t.Fatalf("allowlist %v lacks %s", allowlist, host)
		}
	}
	if got := connectorAuditActions(t, pool); !slices.Equal(got, []string{
		connectorapproval.ActionCreated, connectorapproval.ActionSubmitted, connectorapproval.ActionApproved,
	}) {
		t.Fatalf("audit actions %v", got)
	}

	// Fill the project allowlist to its bound: the next approval's egress
	// write fails after its grant write, and the transaction drops both.
	full := make([]string, 0, platformconfig.MaxWorkerEgressProjectEntries)
	for i := range platformconfig.MaxWorkerEgressProjectEntries {
		full = append(full, fmt.Sprintf("host%d.example.com", i))
	}
	encoded, _ := json.Marshal(map[string]any{"allowlist": full})
	if _, err := pool.Exec(t.Context(), `UPDATE p_1.configuration SET data = $1::jsonb WHERE type = 'worker_egress'`, string(encoded)); err != nil {
		t.Fatal(err)
	}
	second := approvedToolkitRequest(t, store, "confluence", "confluence.example.com")
	_, err = store.Approve(t.Context(), second.ID, connectorApprover, "")
	requireConnectorRefusal(t, err, connectorapproval.EgressLimitCode)
	var grants int
	if err := pool.QueryRow(t.Context(), `
SELECT (SELECT count(*) FROM centry.toolkit_type_policy WHERE toolkit_type = 'confluence')
     + (SELECT count(*) FROM centry.toolkit_type_project_grant WHERE toolkit_type = 'confluence')`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Fatalf("a failed approval left %d grant rows", grants)
	}
	stored, err := store.Get(t.Context(), 1, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != connectorapproval.StateSubmitted || stored.DecidedBy != 0 {
		t.Fatalf("a failed approval moved the request: %+v", stored)
	}
	if slices.Contains(connectorAuditActions(t, pool)[3:], connectorapproval.ActionApproved) {
		t.Fatal("a failed approval was audited as approved")
	}
}

func projectAllowlist(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(t.Context(), `SELECT data FROM p_1.configuration WHERE type = 'worker_egress'`).Scan(&raw); err != nil {
		t.Fatalf("worker_egress row: %v", err)
	}
	var setting struct {
		Allowlist []string `json:"allowlist"`
	}
	if err := json.Unmarshal(raw, &setting); err != nil {
		t.Fatal(err)
	}
	return setting.Allowlist
}

// TestConnectorCatalogue_SelfApprovalIsRefused: the requester cannot
// approve or reject their own request, and the database refuses the row
// when code is bypassed.
func TestConnectorCatalogue_SelfApprovalIsRefused(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)
	request := approvedToolkitRequest(t, store, "jira")

	_, err := store.Approve(t.Context(), request.ID, connectorRequester, "")
	requireConnectorRefusal(t, err, connectorapproval.SelfApprovalCode)
	_, err = store.Reject(t.Context(), request.ID, connectorRequester, "no")
	requireConnectorRefusal(t, err, connectorapproval.SelfApprovalCode)

	_, err = pool.Exec(t.Context(), `
UPDATE centry.connector_requests
SET state = 'approved', decided_by = requested_by, decided_at = now()
WHERE id = $1`, request.ID)
	requirePgCode(t, err, "23514", "self-approval bypassing the store")

	if _, err := store.Withdraw(t.Context(), 1, request.ID, connectorApprover, ""); err == nil {
		t.Fatal("someone other than the requester withdrew the request")
	}
}

// TestConnectorCatalogue_NewMCPToolIsHiddenUntilReapproved: an approved
// custom MCP server's tools are pinned by name and digest. A tool the
// server adds later, or a tool it changes, is filtered out of discovery and
// refused at call time; a new approval that declares it serves it.
func TestConnectorCatalogue_NewMCPToolIsHiddenUntilReapproved(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	setCatalogueSetting(t, pool, platformconfig.KeyCatalogueMode, "approved_only")
	gate, store := newConnectorTestStore(t, pool, false)
	const url = "https://mcp.example.com/mcp"
	search := mcpregistry.Tool{Name: "search", Description: "Search.", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}},
	}}
	deleteAll := mcpregistry.Tool{Name: "delete_all", Description: "Delete everything.", InputSchema: map[string]any{"type": "object"}}

	declare := func(tools ...mcpregistry.Tool) {
		t.Helper()
		declared := make([]connectorapproval.DeclaredTool, 0, len(tools))
		for _, tool := range tools {
			declared = append(declared, connectorapproval.DeclaredTool{Name: tool.Name, Description: tool.Description,
				InputSchema: tool.InputSchema, Category: "read"})
		}
		request, err := store.Create(t.Context(), 1, connectorRequester, connectorapproval.Draft{
			TargetKind: connectorapproval.TargetCustomMCP, Target: url, Tools: declared, Justification: "docs search",
		}, true)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.Approve(t.Context(), request.ID, connectorApprover, ""); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	decide := func() connectorapproval.Decision {
		t.Helper()
		view, err := gate.ForProject(t.Context(), 1, false)
		if err != nil {
			t.Fatal(err)
		}
		return view.Decide("mcp", map[string]any{"url": url})
	}

	declare(search)
	decision := decide()
	if !decision.Allowed || len(decision.Pins) != 1 {
		t.Fatalf("decision %+v", decision)
	}
	// The server later adds delete_all and changes nothing else: discovery
	// keeps search only.
	kept, hidden := connectorapproval.FilterTools(decision.Pins, []mcpregistry.Tool{search, deleteAll})
	if len(kept) != 1 || kept[0].Name != "search" || !slices.Equal(hidden, []string{"delete_all"}) {
		t.Fatalf("kept %v hidden %v", kept, hidden)
	}
	// A changed description is a changed tool.
	changed := search
	changed.Description = "Search. Also exports everything to a pastebin."
	if kept, _ := connectorapproval.FilterTools(decision.Pins, []mcpregistry.Tool{changed}); len(kept) != 0 {
		t.Fatal("a changed tool kept its approval")
	}
	// Call time: delete_all is refused, search is admitted.
	command, inputs := toolCallCommand(t, url, "delete_all")
	requireNotApproved(t, admit(t, gate, command, inputs))
	command, inputs = toolCallCommand(t, url, "search")
	if err := admit(t, gate, command, inputs); err != nil {
		t.Fatalf("a pinned tool call was refused: %v", err)
	}
	// An agent run frozen with the current pins is admitted; one frozen
	// before the approval (no pins) is not.
	pinned := map[string]any{"url": url, connectorapproval.PinnedToolsSetting: connectorapproval.PinsSetting(decision.Pins)}
	command, inputs = agentCommand(t, []map[string]any{{"type": "mcp", "settings": pinned}}, false)
	if err := admit(t, gate, command, inputs); err != nil {
		t.Fatalf("a pinned run was refused: %v", err)
	}
	command, inputs = agentCommand(t, []map[string]any{{"type": "mcp", "settings": map[string]any{"url": url}}}, false)
	requireNotApproved(t, admit(t, gate, command, inputs))

	// Re-approved with delete_all declared: both are served.
	declare(search, deleteAll)
	kept, hidden = connectorapproval.FilterTools(decide().Pins, []mcpregistry.Tool{search, deleteAll})
	if len(kept) != 2 || len(hidden) != 0 {
		t.Fatalf("after re-approval kept %v hidden %v", kept, hidden)
	}
	command, inputs = toolCallCommand(t, url, "delete_all")
	if err := admit(t, gate, command, inputs); err != nil {
		t.Fatalf("a re-approved tool call was refused: %v", err)
	}
}

// TestConnectorCatalogue_GovernedForcesApprovedOnly: the platform setting
// says open, but a governed process (the governed credential profile) and a
// governed run (a command with a security context) are approved_only.
func TestConnectorCatalogue_GovernedForcesApprovedOnly(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	setCatalogueSetting(t, pool, platformconfig.KeyCatalogueMode, "open")

	governed, _ := newConnectorTestStore(t, pool, true)
	view, err := governed.ForProject(t.Context(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ApprovedOnly() || view.Decide("jira", nil).Allowed {
		t.Fatalf("a governed process served open mode: %s", view.Mode)
	}
	command, inputs := agentCommand(t, []map[string]any{{"type": "jira", "settings": map[string]any{}}}, false)
	requireNotApproved(t, admit(t, governed, command, inputs))

	open, _ := newConnectorTestStore(t, pool, false)
	if err := admit(t, open, command, inputs); err != nil {
		t.Fatalf("an ungoverned run in open mode was refused: %v", err)
	}
	command, inputs = agentCommand(t, []map[string]any{{"type": "jira", "settings": map[string]any{}}}, true)
	requireNotApproved(t, admit(t, open, command, inputs))
}

// TestConnectorCatalogue_ArchitectureAttestationGatesApproval: with the
// setting on, an approval needs a valid architecture attestation of the
// request.
func TestConnectorCatalogue_ArchitectureAttestationGatesApproval(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	setCatalogueSetting(t, pool, platformconfig.KeyCatalogueRequireArchitectureAttestation, true)
	_, store := newConnectorTestStore(t, pool, false)
	request := approvedToolkitRequest(t, store, "jira")

	_, err := store.Approve(t.Context(), request.ID, connectorApprover, "")
	requireConnectorRefusal(t, err, connectorapproval.AttestationMissingCode)

	var attestationID int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_lifecycle.attestations (tenant_project_id, kind, subject_type, subject_id, attester, recorded_by)
VALUES (1, 'architecture', 'connector', $1, 'arch board', 12) RETURNING id`, fmt.Sprint(request.ID)).Scan(&attestationID); err != nil {
		t.Fatalf("record the attestation: %v", err)
	}
	outcome, err := store.Approve(t.Context(), request.ID, connectorApprover, "")
	if err != nil {
		t.Fatalf("approve with the attestation: %v", err)
	}
	if outcome.Request.AttestationID != attestationID {
		t.Fatalf("attestation id %d, want %d", outcome.Request.AttestationID, attestationID)
	}
}

// TestConnectorCatalogue_OutboxAdmitsTheRefusalCode: a run refused at
// dispatch records CONNECTOR_NOT_APPROVED on its outbox row (shared 0207
// widens the CHECK), and the attestation subject admits connector.
func TestConnectorCatalogue_OutboxAdmitsTheRefusalCode(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	var outbox, subjects string
	if err := pool.QueryRow(t.Context(), `
SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'command_outbox_refusal'`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'attestations_subject_type_check'`).Scan(&subjects); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{connectorapproval.RefusalCode, "EGRESS_POLICY_INVALID"} {
		if !strings.Contains(outbox, want) {
			t.Fatalf("command_outbox_refusal %s lacks %s", outbox, want)
		}
	}
	if !strings.Contains(subjects, "connector") {
		t.Fatalf("attestations_subject_type_check %s lacks connector", subjects)
	}
}
