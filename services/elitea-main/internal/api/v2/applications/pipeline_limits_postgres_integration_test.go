package applications_test

// The save-time bound on a pipeline definition: 512 KiB of YAML and 128 nodes,
// the numbers the start path and the Worker compiler already enforce
// (internal/domain/pipelinelimits). Every write of pipeline `instructions`
// through the application handler is refused above them with a readable 400;
// reads of an existing over-bound version keep working.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

const limitMarker = "LIMIT-MARKER-DO-NOT-ECHO"

// pipelineNodesYAML builds a pipeline with n nodes (legacy numeric ids, as the
// UI's old editor wrote them), padded with a comment to exactly size bytes
// when size > 0.
func pipelineNodesYAML(n, size int) string {
	var b strings.Builder
	b.WriteString("entry_point: 1\nnodes:\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "  - id: %d\n    type: state_modifier\n    transition: %d\n", i, i+1)
	}
	if size > 0 {
		prefix := "# " + limitMarker + " "
		b.WriteString(prefix)
		if pad := size - b.Len(); pad > 0 {
			b.WriteString(strings.Repeat("x", pad))
		}
	}
	return b.String()
}

func atBytesLimit() string   { return pipelineNodesYAML(1, pipelinelimits.MaxInstructionsBytes) }
func overBytesLimit() string { return pipelineNodesYAML(1, pipelinelimits.MaxInstructionsBytes+1) }

func pipelineCreateBody(name, agentType, instructions string) map[string]any {
	return map[string]any{
		"name": name, "description": "pipeline limits", "type": "pipeline",
		"versions": []any{map[string]any{
			"name": "base", "agent_type": agentType, "instructions": instructions,
			"meta": map[string]any{"step_limit": float64(25)},
		}},
	}
}

type limitsFixture struct {
	pool   *pgxpool.Pool
	router *chi.Mux
}

func newLimitsFixture(t *testing.T) limitsFixture {
	t.Helper()
	pool := newHandlerTestPool(t)
	seedHandlerUser(t, pool, 1, "one@elitea.ai")
	return limitsFixture{pool, newHandlerTestServer(t, pool, auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})}
}

// createPipeline makes a valid small pipeline application and answers its ids.
func (f limitsFixture) createPipeline(t *testing.T, name, agentType string) (appID, versionID string) {
	t.Helper()
	recorder, created := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody(name, agentType, pipelineNodesYAML(2, 0)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed %s: %d %s", name, recorder.Code, recorder.Body.String())
	}
	return created["id"].(string), created["version_details"].(map[string]any)["id"].(string)
}

// requireLimitRefusal asserts the 400 names the limit and echoes nothing.
func requireLimitRefusal(t *testing.T, what string, code int, body string, wantText string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("%s: status = %d, want 400; body=%s", what, code, truncateForLog(body))
	}
	if !strings.Contains(body, wantText) || !strings.Contains(body, "before saving") {
		t.Errorf("%s: body %q does not name the limit and the remedy", what, truncateForLog(body))
	}
	if strings.Contains(body, limitMarker) {
		t.Errorf("%s: the refusal echoes pipeline content", what)
	}
}

func truncateForLog(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func (f limitsFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var n int
	if err := f.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestHandlerPostgres_PipelineLimitsOnCreate(t *testing.T) {
	f := newLimitsFixture(t)

	recorder, _ := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("at-bytes", "pipeline", atBytesLimit()))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("exactly %d bytes: %d %s", pipelinelimits.MaxInstructionsBytes, recorder.Code, truncateForLog(recorder.Body.String()))
	}
	recorder, _ = do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("over-bytes", "pipeline", overBytesLimit()))
	requireLimitRefusal(t, "bytes+1", recorder.Code, recorder.Body.String(), "512 KiB")

	recorder, _ = do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("at-nodes", "pipeline", pipelineNodesYAML(pipelinelimits.MaxNodes, 0)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("%d nodes: %d %s", pipelinelimits.MaxNodes, recorder.Code, truncateForLog(recorder.Body.String()))
	}
	recorder, _ = do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("over-nodes", "pipeline", pipelineNodesYAML(pipelinelimits.MaxNodes+1, 0)))
	requireLimitRefusal(t, "nodes+1", recorder.Code, recorder.Body.String(), "128 nodes")

	// A refused create leaves no application behind.
	for _, name := range []string{"over-bytes", "over-nodes"} {
		if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = $1`, name); n != 0 {
			t.Errorf("refused create %q left %d application rows", name, n)
		}
	}
	for _, name := range []string{"at-bytes", "at-nodes"} {
		if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = $1`, name); n != 1 {
			t.Errorf("accepted create %q stored %d application rows, want 1", name, n)
		}
	}
}

func TestHandlerPostgres_PipelineLimitsOnCreateVersion(t *testing.T) {
	f := newLimitsFixture(t)
	appID, _ := f.createPipeline(t, "cv", "pipeline")
	path := "/versions/prompt_lib/1/" + appID

	for name, tc := range map[string]struct {
		instructions string
		want         int
		text         string
	}{
		"at bytes":   {atBytesLimit(), http.StatusCreated, ""},
		"over bytes": {overBytesLimit(), http.StatusBadRequest, "512 KiB"},
		"at nodes":   {pipelineNodesYAML(pipelinelimits.MaxNodes, 0), http.StatusCreated, ""},
		"over nodes": {pipelineNodesYAML(pipelinelimits.MaxNodes+1, 0), http.StatusBadRequest, "128 nodes"},
	} {
		recorder, _ := do(t, f.router, http.MethodPost, path, map[string]any{
			"name": "v-" + strings.ReplaceAll(name, " ", "-"), "agent_type": "pipeline", "instructions": tc.instructions,
		})
		if tc.want == http.StatusBadRequest {
			requireLimitRefusal(t, name, recorder.Code, recorder.Body.String(), tc.text)
		} else if recorder.Code != tc.want {
			t.Errorf("%s: %d %s", name, recorder.Code, truncateForLog(recorder.Body.String()))
		}
	}
	// Seed + the two accepted ones; the refused ones stored nothing.
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1`, mustAtoi(t, appID)); n != 3 {
		t.Errorf("versions stored = %d, want 3", n)
	}
}

func TestHandlerPostgres_PipelineLimitsOnUpdateVersion(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "uv", "pipeline")
	path := "/version/prompt_lib/1/" + appID + "/" + versionID

	// The stored agent_type decides when the body does not carry one.
	for name, tc := range map[string]struct {
		body map[string]any
		text string
	}{
		"bytes, stored type":       {map[string]any{"instructions": overBytesLimit()}, "512 KiB"},
		"bytes, body type":         {map[string]any{"agent_type": "pipeline", "instructions": overBytesLimit()}, "512 KiB"},
		"nodes, stored type":       {map[string]any{"instructions": pipelineNodesYAML(pipelinelimits.MaxNodes+1, 0)}, "128 nodes"},
		"bytes with name and meta": {map[string]any{"name": "renamed", "meta": map[string]any{"step_limit": float64(25)}, "instructions": overBytesLimit()}, "512 KiB"},
	} {
		recorder, _ := do(t, f.router, http.MethodPut, path, tc.body)
		requireLimitRefusal(t, name, recorder.Code, recorder.Body.String(), tc.text)
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE id = $1 AND name = 'renamed'`, mustAtoi(t, versionID)); n != 0 {
		t.Error("a refused save still applied its other fields")
	}

	for name, instructions := range map[string]string{
		"at bytes": atBytesLimit(), "at nodes": pipelineNodesYAML(pipelinelimits.MaxNodes, 0),
		"unparseable yaml": "nodes: [unterminated\n  - : :",
		"clear":            "",
	} {
		recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"instructions": instructions})
		if recorder.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", name, recorder.Code, truncateForLog(recorder.Body.String()))
		}
	}
}

// Turning a version into a pipeline bounds the instructions it already stores,
// even when the request carries none.
func TestHandlerPostgres_PipelineLimitsOnAgentTypeChange(t *testing.T) {
	f := newLimitsFixture(t)
	big := strings.Repeat("a", pipelinelimits.MaxInstructionsBytes+1)
	recorder, created := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("flip-big", "openai", big))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed agent: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	path := "/version/prompt_lib/1/" + created["id"].(string) + "/" + created["version_details"].(map[string]any)["id"].(string)
	recorder, _ = do(t, f.router, http.MethodPut, path, map[string]any{"agent_type": "pipeline"})
	requireLimitRefusal(t, "flip over-bound agent to pipeline", recorder.Code, recorder.Body.String(), "512 KiB")
	if n := f.count(t, `SELECT count(*) FROM p_1.application_versions WHERE agent_type = 'pipeline' AND octet_length(instructions) > $1`, pipelinelimits.MaxInstructionsBytes); n != 0 {
		t.Errorf("a refused type change stored %d over-bound pipelines", n)
	}

	appID, versionID := f.createPipeline(t, "flip-small", "openai")
	recorder, _ = do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/"+versionID, map[string]any{"agent_type": "pipeline"})
	if recorder.Code != http.StatusCreated {
		t.Errorf("flip in-bound agent to pipeline: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

// An agent is not a pipeline: its instructions are prose and are not bounded here.
func TestHandlerPostgres_PipelineLimitsDoNotApplyToAgents(t *testing.T) {
	f := newLimitsFixture(t)
	big := strings.Repeat("a", pipelinelimits.MaxInstructionsBytes+1)

	recorder, created := do(t, f.router, http.MethodPost, "/applications/prompt_lib/1",
		pipelineCreateBody("agent-create", "openai", big))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("agent create: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	appID := created["id"].(string)
	versionID := created["version_details"].(map[string]any)["id"].(string)

	if recorder, _ = do(t, f.router, http.MethodPost, "/versions/prompt_lib/1/"+appID,
		map[string]any{"name": "v2", "agent_type": "openai", "instructions": big}); recorder.Code != http.StatusCreated {
		t.Errorf("agent create version: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	if recorder, _ = do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/"+versionID,
		map[string]any{"instructions": big + "b"}); recorder.Code != http.StatusCreated {
		t.Errorf("agent update version (stored type): %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	// Prose that happens to parse as a long `nodes` list is still prose for an agent.
	if recorder, _ = do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/"+versionID,
		map[string]any{"instructions": pipelineNodesYAML(pipelinelimits.MaxNodes+1, 0)}); recorder.Code != http.StatusCreated {
		t.Errorf("agent update version (nodes): %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

// The application-level PUT carries a nested `version` stub whose instructions
// are written through the same repository call.
func TestHandlerPostgres_PipelineLimitsOnApplicationUpdate(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "au", "pipeline")

	recorder, _ := do(t, f.router, http.MethodPut, "/application/prompt_lib/1/"+appID, map[string]any{
		"version": map[string]any{"application_id": appID, "id": versionID, "instructions": overBytesLimit()},
	})
	requireLimitRefusal(t, "application update", recorder.Code, recorder.Body.String(), "512 KiB")
	// The refusal rolls the whole request back, so the application fields did not change either.
	recorder, _ = do(t, f.router, http.MethodPut, "/application/prompt_lib/1/"+appID, map[string]any{
		"name":    "au-renamed",
		"version": map[string]any{"application_id": appID, "id": versionID, "instructions": overBytesLimit()},
	})
	requireLimitRefusal(t, "application update with a rename", recorder.Code, recorder.Body.String(), "512 KiB")
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'au-renamed'`); n != 0 {
		t.Error("a refused application update still renamed the application")
	}

	recorder, _ = do(t, f.router, http.MethodPut, "/application/prompt_lib/1/"+appID, map[string]any{
		"version": map[string]any{"application_id": appID, "id": versionID, "instructions": pipelineNodesYAML(3, 0)},
	})
	if recorder.Code != http.StatusCreated {
		t.Errorf("in-bound application update: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

// A version write that only the database refuses (a NUL byte is not storable in
// text) fails after the application update succeeded in the same request. Both
// writes share one transaction, so the rename is rolled back and the request
// answers a client error, not a 201.
func TestHandlerPostgres_ApplicationUpdateRollsBackWhenVersionWriteFails(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "rb", "pipeline")

	recorder, _ := do(t, f.router, http.MethodPut, "/application/prompt_lib/1/"+appID, map[string]any{
		"name":    "rb-renamed",
		"version": map[string]any{"application_id": appID, "id": versionID, "instructions": "nodes:\x00"},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	if !strings.Contains(recorder.Body.String(), "cannot be stored") {
		t.Errorf("the refusal does not say why: %s", truncateForLog(recorder.Body.String()))
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'rb-renamed'`); n != 0 {
		t.Error("a failed version write left the application renamed")
	}
	if n := f.count(t, `SELECT count(*) FROM p_1.applications WHERE name = 'rb'`); n != 1 {
		t.Error("the application lost its original name")
	}
}

// A version stored above the bound (local version 133 is about 8 MB) keeps
// loading and listing; only a write of over-bound instructions is refused, and
// a save that carries no instructions is not.
func TestHandlerPostgres_ExistingOverBoundVersionStaysReadableAndEditable(t *testing.T) {
	f := newLimitsFixture(t)
	appID, versionID := f.createPipeline(t, "legacy", "pipeline")
	huge := pipelineNodesYAML(1, 3*pipelinelimits.MaxInstructionsBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE p_1.application_versions SET instructions = $1 WHERE id = $2`, huge, mustAtoi(t, versionID)); err != nil {
		t.Fatalf("seed over-bound version: %v", err)
	}

	for name, req := range map[string]struct{ method, path string }{
		"get version":     {http.MethodGet, "/version/prompt_lib/1/" + appID + "/" + versionID},
		"list versions":   {http.MethodGet, "/versions/prompt_lib/1/" + appID},
		"get application": {http.MethodGet, "/application/prompt_lib/1/" + appID},
		"list apps":       {http.MethodGet, "/applications/prompt_lib/1"},
		"default version": {http.MethodGet, "/default_version/prompt_lib/1/" + appID},
	} {
		recorder, _ := do(t, f.router, req.method, req.path, nil)
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: %d %s", name, recorder.Code, truncateForLog(recorder.Body.String()))
		}
	}
	_, got := do(t, f.router, http.MethodGet, "/version/prompt_lib/1/"+appID+"/"+versionID, nil)
	if instr, _ := got["instructions"].(string); len(instr) != len(huge) {
		t.Errorf("read %d bytes of instructions, want the stored %d", len(instr), len(huge))
	}

	path := "/version/prompt_lib/1/" + appID + "/" + versionID
	if recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"name": "renamed", "welcome_message": "hi"}); recorder.Code != http.StatusCreated {
		t.Errorf("a save without instructions was refused: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
	recorder, _ := do(t, f.router, http.MethodPut, path, map[string]any{"instructions": huge})
	requireLimitRefusal(t, "writing the over-bound text back", recorder.Code, recorder.Body.String(), "512 KiB")
	if recorder, _ = do(t, f.router, http.MethodPut, path, map[string]any{"instructions": pipelineNodesYAML(2, 0)}); recorder.Code != http.StatusCreated {
		t.Errorf("shrinking the version was refused: %d %s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

// A write to a version that does not exist answers 404, never the size error:
// the stored type lookup finds nothing and the refusal is not guessed.
func TestHandlerPostgres_PipelineLimitOnUnknownVersionIsNotFound(t *testing.T) {
	f := newLimitsFixture(t)
	appID, _ := f.createPipeline(t, "nf", "pipeline")
	recorder, _ := do(t, f.router, http.MethodPut, "/version/prompt_lib/1/"+appID+"/999999",
		map[string]any{"instructions": overBytesLimit()})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, truncateForLog(recorder.Body.String()))
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return n
}
