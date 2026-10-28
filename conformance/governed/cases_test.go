//go:build conformance

package governed_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The cases of issue #1124. Every subtest name starts with its GOV id, so a
// report, the declared-skip ledger and EVIDENCE.md all name the same thing.
//
// The governance flag is set by the overlay and the script, never here: the
// suite drives the deployment only through what an operator and a user can
// reach (public API, operator CLI) and reads the database only to observe.

// TestGovernedFlagOff is GOV-001's stack leg: with the governance flag off
// (phase off) the governed surface does not exist and the legacy path works.
// The byte-identical golden vectors are the unit leg (package golden).
func TestGovernedFlagOff(t *testing.T) {
	ctx := testContext(t, 10*time.Minute)
	w := newWorld(ctx, t, phaseOff)

	t.Run("GOV-001/governed_routes_are_not_mounted", func(t *testing.T) {
		for _, path := range []string{
			"/api/v2/admin/agent_identities",
			"/api/v2/admin/mandates",
			"/api/v2/admin/external_agent_identities",
			"/api/v2/admin/credential_references?project_id=" + w.p(),
			"/api/v2/projects/" + w.p() + "/credential_grants",
		} {
			w.admin.do(ctx, t, http.MethodGet, path, nil, http.StatusNotFound)
		}
		// Unauthenticated: the protected-resource metadata is public when it
		// exists, so a 404 here is the route's absence, not an auth refusal.
		anonymous := &persona{origin: w.cfg.Origin, http: w.admin.http}
		anonymous.do(ctx, t, http.MethodGet, "/.well-known/oauth-protected-resource/app/"+w.p()+"/mcp", nil, http.StatusNotFound)
	})

	t.Run("GOV-001/legacy_toolkit_turn_works", func(t *testing.T) {
		// NOT a canary: the legacy path carries the secret in the run's
		// input, which is exactly what the governed profile removes.
		token := "legacy-jira-token-" + shortID() + shortID()
		toolkitID := w.jiraToolkit(ctx, t, w.admin, "legacy", token, []string{"list_projects"})
		applicationID, versionID := w.agent(ctx, t, w.admin, "legacy")
		w.attachToolkit(ctx, t, w.admin, toolkitID, applicationID, versionID)
		fixture := w.converse(ctx, t, w.user, turnFixture{ApplicationID: applicationID, VersionID: versionID,
			ToolkitID: toolkitID, CredentialSHA256: sha(token)})
		before := len(w.cfg.callsWith(ctx, t, "jira.governed.test", token))
		w.mustAdmit(ctx, t, w.user, fixture, callTool("list_projects", ""))
		last := w.settled(ctx, t, w.user, fixture, 4*time.Minute)
		if !strings.Contains(messageRaw(last), "GOVJIRASENTINEL") {
			t.Fatalf("the legacy turn did not reach the provider: %s", messageRaw(last))
		}
		if after := len(w.cfg.callsWith(ctx, t, "jira.governed.test", token)); after <= before {
			t.Fatal("the provider never saw the legacy token")
		}
		w.cfg.updateState(t, func(s *state) { s.Legacy = &fixture })
	})
}

// TestGovernedProfile is GOV-002 to GOV-010 with the governance flag on and
// the governed worker (phase on).
func TestGovernedProfile(t *testing.T) {
	ctx := testContext(t, 45*time.Minute)
	w := newWorld(ctx, t, phaseOn)

	t.Run("GOV-002", func(t *testing.T) {
		// Declared in scripts/go/declared-skips.txt.
		t.Skip("gate D1/entra: an Entra-delegated interactive run needs an Entra sign-in that mints a delegation, " +
			"and the delegation is sealed with SECRETS_MASTER_KEY, which the standalone stack leaves unset; " +
			"unit legs: internal/entra delegation and broker OBO tests (EVIDENCE.md)")
	})

	t.Run("GOV-003/pgvault_static_secret", func(t *testing.T) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov003-pg", MandateFor: []*persona{w.user}})
		fixture := w.converse(ctx, t, w.user, agent.turnFixture)
		w.expectUsed(ctx, t, w.user, agent, fixture, callTool("list_projects", ""), "GOVJIRASENTINEL")
		// Idempotent: a second apply finds what it staged and changes nothing.
		for _, line := range w.migrateReferences(ctx, t, agent.ToolkitID, true) {
			if line.Action != "exists" || line.CredentialReferenceID != agent.Reference.CredentialReferenceID {
				t.Fatalf("a re-run of migrate-references is not idempotent: %+v", line)
			}
		}
	})

	t.Run("GOV-003/vault_static_secret", func(t *testing.T) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov003-vault", MandateFor: []*persona{w.user}, Backend: "vault"})
		want := fmt.Sprintf("elitea/projects/%d/credentials/%s", w.cfg.Project, agent.Reference.CredentialReferenceID)
		if agent.Reference.SecretLocation != want && agent.Reference.Locator != want {
			t.Fatalf("the vault copy is not at %s: %+v", want, agent.Reference)
		}
		// The value is in OpenBao, written by the CLI, read here only to
		// compare (never printed).
		stored, _, err := w.cfg.stack(ctx, "", "exec", "-T", "-e", "BAO_ADDR=http://127.0.0.1:8201",
			"-e", "BAO_TOKEN=elitea-governed-root", "openbao", "bao", "kv", "get", "-mount=kv", "-field=value", want)
		if err != nil || strings.TrimSpace(stored) != agent.Token {
			t.Fatalf("OpenBao does not hold the migrated secret at %s (%v)", want, err)
		}
		fixture := w.converse(ctx, t, w.user, agent.turnFixture)
		w.expectUsed(ctx, t, w.user, agent, fixture, callTool("list_projects", ""), "GOVJIRASENTINEL")
	})

	t.Run("GOV-004", func(t *testing.T) {
		t.Skip("gate synthetic-adapter: the shipped worker registers no synthetic sixth adapter; " +
			"proven by unit tests a_sixth_adapter_registers_and_serves_a_governed_call and " +
			"TestSyntheticAdapterRegistersBesideTheBuiltins (EVIDENCE.md)")
	})

	t.Run("GOV-005", func(t *testing.T) { gov005(ctx, t, w) })
	t.Run("GOV-006", func(t *testing.T) { gov006(ctx, t, w) })
	t.Run("GOV-008", func(t *testing.T) { gov008(ctx, t, w) })
	t.Run("GOV-009", func(t *testing.T) { gov009(ctx, t, w) })
	t.Run("GOV-010", func(t *testing.T) { gov010(ctx, t, w) })

	t.Run("GOV-007/prepare", func(t *testing.T) {
		// The agent the old-worker phase sends to. Prepared here, with the
		// governance flag on, so the next phase only restarts the worker.
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov007", MandateFor: []*persona{w.user}})
		fixture := w.converse(ctx, t, w.user, agent.turnFixture)
		w.cfg.updateState(t, func(s *state) { s.OldWorker = &fixture })
	})
}

// gov005: each gate alone denies. One fixture per gate, so each subtest
// breaks exactly one thing.
func gov005(ctx context.Context, t *testing.T, w *world) {
	t.Run("auth_source", func(t *testing.T) {
		t.Skip("gate auth-source: every way to start a run on this stack is an allowed auth source " +
			"(PAT and session); a development or unknown source is unit-proven by " +
			"TestDefaultAuthSourcesRefuseUnknownAndDevelopment (EVIDENCE.md)")
	})
	t.Run("entity_access", func(t *testing.T) {
		t.Skip("gate entity-access: on this stack project membership is the RBAC gate's own precondition, " +
			"so no persona passes RBAC and fails entity access alone; unit-proven by " +
			"TestDecideEachGateAloneDenies/entity_access (EVIDENCE.md)")
	})

	t.Run("rbac", func(t *testing.T) {
		// The member holds every chat permission in the project but not
		// models.credentials.governed.use, and has its own mandate, so only
		// RBAC can deny.
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov005-rbac", MandateFor: []*persona{w.member}})
		fixture := w.converse(ctx, t, w.member, agent.turnFixture)
		result := w.send(ctx, t, w.member, fixture, callTool("list_projects", ""))
		agent.turnFixture = fixture
		w.expectDenied(ctx, t, w.member, agent, result, []string{"permission_missing"}, []string{"permission_missing"})
	})

	t.Run("binding", func(t *testing.T) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov005-binding", MandateFor: []*persona{w.user}})
		w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/agent_identities/"+agent.BindingID+":suspend", map[string]any{}, http.StatusOK)
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.send(ctx, t, w.user, agent.turnFixture, callTool("list_projects", ""))
		w.expectDenied(ctx, t, w.user, agent, result,
			[]string{"binding_not_active", "agent_binding_not_active", "agent_binding_required"},
			[]string{"binding_not_active", "authority_not_active"})
	})

	t.Run("toolkit", func(t *testing.T) {
		// The binding was pinned BEFORE the reference was attached, so the
		// toolkit's digest has moved since.
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov005-toolkit", MandateFor: []*persona{w.user}, BindBeforeAttach: true})
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.send(ctx, t, w.user, agent.turnFixture, callTool("list_projects", ""))
		w.expectDenied(ctx, t, w.user, agent, result, []string{"toolkit_digest_mismatch"}, []string{"toolkit_digest_mismatch"})
		// Re-pinning the current digest admits the same agent.
		w.bindToolkit(ctx, t, agent.BindingID, agent.ToolkitID)
		w.expectUsed(ctx, t, w.user, agent, agent.turnFixture, callTool("list_projects", ""), "GOVJIRASENTINEL")
	})

	t.Run("grant", func(t *testing.T) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov005-grant", MandateFor: []*persona{w.user}})
		w.admin.do(ctx, t, http.MethodPost, "/api/v2/projects/"+w.p()+"/credential_grants/"+agent.GrantID+":revoke", map[string]any{}, http.StatusOK)
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.send(ctx, t, w.user, agent.turnFixture, callTool("list_projects", ""))
		w.expectDenied(ctx, t, w.user, agent, result, []string{"credential_grant_missing"}, []string{"grant_missing", "grant_not_active"})
	})

	t.Run("operation", func(t *testing.T) {
		// The grant covers a different operation than the one called.
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov005-operation", MandateFor: []*persona{w.user},
			GrantOperations: []string{"jira.issue.read"}})
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.send(ctx, t, w.user, agent.turnFixture, callTool("list_projects", ""))
		w.expectDenied(ctx, t, w.user, agent, result, []string{"credential_grant_missing"}, []string{"operation_out_of_scope"})
	})

	t.Run("authority", func(t *testing.T) {
		// No delegation and no mandate names the user.
		agent := w.governedAgent(ctx, t, agentSpec{Label: "gov005-authority"})
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.send(ctx, t, w.user, agent.turnFixture, callTool("list_projects", ""))
		w.expectDenied(ctx, t, w.user, agent, result,
			[]string{"authority_missing", "mandate_required", "authority_required"},
			[]string{"authority_missing", "authority_not_active"})
	})
}

// gov006: revocation reaches a run that is already admitted.
func gov006(ctx context.Context, t *testing.T, w *world) {
	// A sensitive call (add_comments, worker-toolkit-security.json) pauses
	// before the credential is redeemed; the revocation lands in the pause.
	duringPause := func(t *testing.T, label string, revoke func(agent governedAgent)) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: label, MandateFor: []*persona{w.user}})
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.mustAdmit(ctx, t, w.user, agent.turnFixture,
			callTool("add_comments", `{"issue_key":"GOV-1","comment":"governed conformance"}`))
		paused := w.interrupt(ctx, t, w.user, agent.turnFixture, result.ResponseMessageID)
		before := len(w.cfg.callsWith(ctx, t, "jira.governed.test", agent.Token))
		revoke(agent)
		if answer := w.resume(ctx, t, w.user, paused); answer.Status >= 500 {
			t.Fatalf("the resume failed rather than being decided: %s", answer)
		}
		w.settled(ctx, t, w.user, agent.turnFixture, 4*time.Minute)
		if after := len(w.cfg.callsWith(ctx, t, "jira.governed.test", agent.Token)); after != before {
			t.Fatalf("the provider saw the credential after the revocation (%d calls)", after-before)
		}
		if n := w.redemptions(ctx, t, result.ExecutionID); n != 0 {
			t.Fatalf("execution %s redeemed %d credentials after the revocation", result.ExecutionID, n)
		}
	}

	t.Run("grant_revoked_during_hitl", func(t *testing.T) {
		duringPause(t, "gov006-grant", func(agent governedAgent) {
			w.admin.do(ctx, t, http.MethodPost, "/api/v2/projects/"+w.p()+"/credential_grants/"+agent.GrantID+":revoke", map[string]any{}, http.StatusOK)
		})
	})

	t.Run("reference_retired_during_hitl", func(t *testing.T) {
		duringPause(t, "gov006-reference", func(agent governedAgent) {
			retired := w.admin.do(ctx, t, http.MethodPost, referencesPath+agent.Reference.CredentialReferenceID+":retire", map[string]any{}, http.StatusOK).Object(t)
			if retired["state"] != "retired" {
				t.Fatalf("a retired reference reads %v", retired["state"])
			}
			// Retiring does not bump the generation (the reference is not
			// rotated; it stops being usable).
			if generation, _ := retired["generation"].(float64); int64(generation) != agent.Reference.Generation {
				t.Fatalf("retiring changed the generation: %v, staged %d", retired["generation"], agent.Reference.Generation)
			}
		})
	})

	// A long stream (`[[mock:slow]]`) keeps the run holding its lease; the
	// revocation must end it at the next renewal, before the stream's end
	// sentinel is ever written.
	duringStream := func(t *testing.T, label string, revoke func(agent governedAgent)) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: label, MandateFor: []*persona{w.user}})
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		w.mustAdmit(ctx, t, w.user, agent.turnFixture, "[[mock:slow]] governed revocation")
		time.Sleep(10 * time.Second)
		revoke(agent)
		last := w.settled(ctx, t, w.user, agent.turnFixture, 4*time.Minute)
		if strings.Contains(messageRaw(last), "MOCKSTREAMEND") {
			t.Fatal("the run streamed to its end after its authority was revoked")
		}
	}

	t.Run("binding_revoked_during_stream", func(t *testing.T) {
		duringStream(t, "gov006-binding", func(agent governedAgent) {
			w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/agent_identities/"+agent.BindingID+":revoke", map[string]any{}, http.StatusOK)
		})
	})

	t.Run("mandate_revoked_during_stream", func(t *testing.T) {
		duringStream(t, "gov006-mandate", func(agent governedAgent) {
			w.admin.do(ctx, t, http.MethodPost, "/api/v2/admin/mandates/"+agent.MandateIDs[0]+":revoke", map[string]any{}, http.StatusOK)
		})
	})

	t.Run("delegation_revoked", func(t *testing.T) {
		t.Skip("gate D1/entra: no delegation exists on this stack (see GOV-002); the delegation epoch is " +
			"unit-proven by TestStaleEpochsAndDigestsDeny (EVIDENCE.md)")
	})
}

// gov008: a schedule runs under the mandate attached to it and nothing else.
func gov008(ctx context.Context, t *testing.T, w *world) {
	applicationID, versionID := w.pipeline(ctx, t, w.admin, "gov008")
	bindingID := w.bindAgent(ctx, t, applicationID, versionID)
	schedulePath := fmt.Sprintf("/api/v2/pipeline_schedules/prompt_lib/%s/%d", w.p(), versionID)
	attachPath := fmt.Sprintf("/api/v2/admin/mandate_attachments/pipeline_schedules/%s/%d", w.p(), versionID)
	w.admin.do(ctx, t, http.MethodPut, schedulePath,
		map[string]any{"cron": "* * * * *", "active": true, "input": "GOV-008 scheduled run"}, http.StatusOK, http.StatusCreated)

	waitResult := func(t *testing.T, want string) map[string]any {
		var schedule map[string]any
		waitCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		eventually(waitCtx, t, 5*time.Second, "last_result "+want, func() bool {
			schedule = w.admin.do(waitCtx, t, http.MethodGet, schedulePath, nil, http.StatusOK).Object(t)
			return schedule["last_result"] == want
		})
		return schedule
	}

	t.Run("no_mandate_no_run", func(t *testing.T) {
		waitResult(t, "skipped_mandate_required")
	})

	t.Run("mandate_scope_must_cover_the_cron", func(t *testing.T) {
		narrow := w.mandate(ctx, t, "autonomous", bindingID, nil, []string{"jira.project.read"}, []string{"schedule.cron:0_0_1_1_*"})
		refused := w.admin.do(ctx, t, http.MethodPut, attachPath, map[string]any{"mandate_id": narrow}, http.StatusConflict).Object(t)
		if refused["code"] != "MANDATE_SCOPE_MISMATCH" {
			t.Fatalf("an out-of-scope mandate was refused with %v", refused)
		}
	})

	t.Run("attached_mandate_runs_governed", func(t *testing.T) {
		covering := w.mandate(ctx, t, "autonomous", bindingID, nil, []string{"jira.project.read"}, []string{"schedule.cron:*_*_*_*_*"})
		w.admin.do(ctx, t, http.MethodPut, attachPath, map[string]any{"mandate_id": covering}, http.StatusOK)
		schedule := waitResult(t, "dispatched")
		executionID, _ := schedule["last_execution_id"].(string)
		if executionID == "" {
			t.Fatalf("a dispatched schedule names no execution: %v", schedule)
		}
		if profile := w.admissionProfile(ctx, t, executionID); profile != governedProfile {
			t.Fatalf("the scheduled run %s was pinned to %s", executionID, profile)
		}
		// Stop it firing for the rest of the run.
		w.admin.do(ctx, t, http.MethodDelete, schedulePath, nil, http.StatusOK, http.StatusNoContent)
	})
}

// gov009: governed MCP. Inbound: the protected-resource metadata and the
// exact audience. Outbound needs provider OAuth, which needs the vault key.
func gov009(ctx context.Context, t *testing.T, w *world) {
	anonymous := &persona{origin: w.cfg.Origin, http: w.admin.http}
	endpoint := "/app/" + w.p() + "/mcp"

	t.Run("inbound", func(t *testing.T) {
		metadata := anonymous.do(ctx, t, http.MethodGet, "/.well-known/oauth-protected-resource"+endpoint, nil, http.StatusOK).Object(t)
		resource, _ := metadata["resource"].(string)
		if !strings.HasPrefix(resource, "https://") || !strings.HasSuffix(resource, endpoint) {
			t.Fatalf("the protected resource is not the exact https endpoint: %v", metadata)
		}
		servers, _ := metadata["authorization_servers"].([]any)
		if len(servers) == 0 {
			t.Fatalf("the metadata names no authorization server: %v", metadata)
		}
		initialize := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "governed-conformance", "version": "1"}}}
		call := func(token string) response {
			header := http.Header{"Accept": {"application/json, text/event-stream"}}
			if token != "" {
				header.Set("Authorization", "Bearer "+token)
			}
			return anonymous.raw(ctx, t, http.MethodPost, endpoint, initialize, header)
		}
		challenge := func(t *testing.T, answer response, why string) {
			if answer.Status != http.StatusUnauthorized ||
				!strings.Contains(answer.Header.Get("WWW-Authenticate"), "resource_metadata=") {
				t.Fatalf("%s: want 401 with resource_metadata, got %s (WWW-Authenticate %q)", why, answer,
					answer.Header.Get("WWW-Authenticate"))
			}
		}
		challenge(t, call(""), "no token")
		challenge(t, call(w.cfg.mintInboundMCPToken(ctx, t, map[string]any{
			"audience": resource + "/other", "email": w.cfg.User, "scope": "mcp.invoke"})), "a token for another audience")
		challenge(t, call(w.cfg.mintInboundMCPToken(ctx, t, map[string]any{
			"audience": resource, "email": w.cfg.User, "scope": "mcp.invoke", "untrusted_key": true})), "a token the issuer did not sign")
		good := call(w.cfg.mintInboundMCPToken(ctx, t, map[string]any{
			"audience": resource, "email": w.cfg.User, "scope": "mcp.invoke"}))
		if good.Status == http.StatusUnauthorized || good.Status >= 500 {
			t.Fatalf("a token for the exact endpoint was not accepted: %s", good)
		}
	})

	t.Run("outbound", func(t *testing.T) {
		t.Skip("gate vault-key: outbound MCP OAuth stores the provider grant sealed with SECRETS_MASTER_KEY, " +
			"which the standalone stack leaves unset; unit-proven by mcp_governed_tests.rs and the governed MCP " +
			"client tests (EVIDENCE.md)")
	})
}

// gov010: the audit chain holds under concurrency.
func gov010(ctx context.Context, t *testing.T, w *world) {
	agent := w.governedAgent(ctx, t, agentSpec{Label: "gov010"})
	count := func() int64 {
		return w.cfg.psqlInt(ctx, t, `SELECT count(*) FROM elitea_runtime.security_audit_outbox
 WHERE event_type = 'ai.elitea.security.v1.probe' AND partition_key = :'p';`, map[string]string{"p": w.p()})
	}
	before := count()
	const probes = 8
	var wg sync.WaitGroup
	statuses := make([]int, probes)
	for i := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = w.admin.raw(ctx, t, http.MethodPost,
				"/api/v2/projects/"+w.p()+"/credential_references/"+agent.Reference.CredentialReferenceID+":probe", map[string]any{}, nil).Status
		}()
	}
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("probe %d answered %d", i, status)
		}
	}
	if after := count(); after-before != probes {
		t.Fatalf("%d concurrent probes appended %d audit events", probes, after-before)
	}
	for _, source := range []string{"outbox", "events"} {
		// The outbox drains to the durable store asynchronously.
		verifyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		eventually(verifyCtx, t, 5*time.Second, "security-audit verify --source "+source, func() bool {
			_, _, err := w.cfg.mainCLI(verifyCtx, "security-audit", "verify", "--tenant", w.p(), "--source", source)
			return err == nil
		})
		cancel()
	}
	if n := w.cfg.psqlInt(ctx, t, `SELECT count(*) FROM elitea_identity.security_audit_events
 WHERE event_type = 'ai.elitea.security.v1.probe' AND partition_key = :'p';`, map[string]string{"p": w.p()}); n < probes {
		t.Fatalf("the durable store holds %d probe events, want at least %d", n, probes)
	}
}
