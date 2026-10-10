package material_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// fakeGrants admits exactly one (project, user, token, toolkit, provider,
// tool) and records every lookup.
type fakeGrants struct {
	err     error
	lookups int
}

func (f *fakeGrants) AdmitsSourceTool(
	_ context.Context, projectID, userID, tokenID, toolkitID int64, provider, tool string,
) (bool, error) {
	f.lookups++
	if f.err != nil {
		return false, f.err
	}
	return projectID == 42 && userID == 11 && tokenID == 900 && toolkitID == 101 &&
		provider == "inventory" && tool == "investigate", nil
}

// gate composes SourceToolGate over two stand-in permission middlewares that
// answer 200 "patch" / "execute" when the caller holds them and 403
// otherwise, and a terminal handler that echoes the body it received.
func gate(grants material.SourceToolGrants, holds ...string) http.Handler {
	permission := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !slices.Contains(holds, name) {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, "denied by "+name)
					return
				}
				w.Header().Set("X-Gate", name)
				next.ServeHTTP(w, r)
			})
		}
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
	router := chi.NewRouter()
	router.With(material.SourceToolGate(grants, "inventory", "investigate",
		material.ReadOnlySourceTool, permission("tool.patch"), permission("tool.execute"), nil)).
		Post("/test_tool/prompt_lib/{projectID}/{toolID}", echo)
	return router
}

// engineBody is exactly what the Inventory engine sends (native.rs
// source_caller): the gate allow-lists this shape.
const engineBody = `{"request_id":"investigate-101-get_issue","tool_name":"get_issue",` +
	`"tool_params":{"issue_number":7},"toolkit_config":{"toolkit_id":"101"}}`

func callbackUser() auth.User {
	bound := int64(42)
	return auth.User{ID: "11", UserID: "11", TokenID: "900", AuthType: "token", TokenProjectID: &bound}
}

func serve(t *testing.T, handler http.Handler, path, body string, user *auth.User) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestTheInvestigateGrantPassesWithExecuteAndKeepsTheBody(t *testing.T) {
	grants := &fakeGrants{}
	user := callbackUser()
	response := serve(t, gate(grants, "tool.execute"),
		"/test_tool/prompt_lib/42/101", engineBody, &user)
	if response.Code != http.StatusOK || response.Header().Get("X-Gate") != "tool.execute" {
		t.Fatalf("want the execute gate to admit, got %d %q: %s",
			response.Code, response.Header().Get("X-Gate"), response.Body.String())
	}
	if response.Body.String() != engineBody {
		t.Errorf("the handler did not receive the body the gate read: %s", response.Body.String())
	}
	if grants.lookups != 1 {
		t.Errorf("want one grant lookup, got %d", grants.lookups)
	}
}

func TestTheInvestigateGrantStillNeedsExecute(t *testing.T) {
	user := callbackUser()
	response := serve(t, gate(&fakeGrants{}), "/test_tool/prompt_lib/42/101", engineBody, &user)
	if response.Code != http.StatusForbidden || response.Body.String() != "denied by tool.execute" {
		t.Fatalf("a grant without execute must be refused by execute, got %d: %s",
			response.Code, response.Body.String())
	}
}

// Every request the grant does not cover takes the route's own gate, and a
// caller without patch is refused there.
func TestEverythingElseTakesThePatchGate(t *testing.T) {
	session := auth.User{ID: "11", UserID: "11", AuthType: "session"}
	pat := auth.User{ID: "11", UserID: "11", TokenID: "901", AuthType: "token"}
	native := callbackUser()
	native.NativeClientID = "desktop"
	boundElsewhere := callbackUser()
	other := int64(43)
	boundElsewhere.TokenProjectID = &other
	write := strings.Replace(engineBody, `"get_issue"`, `"create_issue"`, 1)
	branchy := strings.Replace(engineBody, `"get_issue"`, `"list_branches_in_repo"`, 1)
	padded := strings.Replace(engineBody, `"get_issue"`, `"  delete_file  "`, 1)
	// add puts one more top-level key into the engine's body.
	add := func(field string) string {
		return strings.Replace(engineBody, `"request_id"`, field+`,"request_id"`, 1)
	}
	withSettings := add(`"llm_settings":{"api_key":"x"}`)
	withMCP := add(`"mcp_authorization_reference":"r"`)
	withModel := add(`"llm_model":"gpt-x"`)
	withFoldedModel := add(`"LLM_Model":"gpt-x"`)
	withConfiguration := add(`"llm_configuration":{"x":1}`)
	withTokens := add(`"mcp_tokens":{"x":"y"}`)
	withUnknown := add(`"future_field":1`)
	configExtra := strings.Replace(engineBody, `{"toolkit_id":"101"}`, `{"toolkit_id":"101","settings":{"token":"x"}}`, 1)
	configNotObject := strings.Replace(engineBody, `{"toolkit_id":"101"}`, `"101"`, 1)
	// encoding/json matches keys case-insensitively and the last one wins:
	// the name checked must be the name test_tool would run.
	shadowed := add(`"Tool_Name":"update_issue"`)
	shadowedAfter := strings.Replace(engineBody, `}}`, `},"Tool_Name":"update_issue"}`, 1)
	callback := callbackUser()

	cases := map[string]struct {
		user *auth.User
		path string
		body string
	}{
		"a session":                          {&session, "/test_tool/prompt_lib/42/101", engineBody},
		"a plain PAT":                        {&pat, "/test_tool/prompt_lib/42/101", engineBody},
		"a native client credential":         {&native, "/test_tool/prompt_lib/42/101", engineBody},
		"a token bound to another project":   {&boundElsewhere, "/test_tool/prompt_lib/42/101", engineBody},
		"the callback for another project":   {&callback, "/test_tool/prompt_lib/43/101", engineBody},
		"a toolkit the grant does not name":  {&callback, "/test_tool/prompt_lib/42/102", engineBody},
		"a write tool":                       {&callback, "/test_tool/prompt_lib/42/101", write},
		"a read prefix with a write pattern": {&callback, "/test_tool/prompt_lib/42/101", branchy},
		"a write tool behind whitespace":     {&callback, "/test_tool/prompt_lib/42/101", padded},
		"caller-supplied llm_settings":       {&callback, "/test_tool/prompt_lib/42/101", withSettings},
		"an MCP authorization reference":     {&callback, "/test_tool/prompt_lib/42/101", withMCP},
		"a case-folded duplicate tool name":  {&callback, "/test_tool/prompt_lib/42/101", shadowed},
		"a case-folded tool name last":       {&callback, "/test_tool/prompt_lib/42/101", shadowedAfter},
		"a caller-chosen llm_model":          {&callback, "/test_tool/prompt_lib/42/101", withModel},
		"a case-folded llm_model":            {&callback, "/test_tool/prompt_lib/42/101", withFoldedModel},
		"an inline llm_configuration":        {&callback, "/test_tool/prompt_lib/42/101", withConfiguration},
		"inline mcp_tokens":                  {&callback, "/test_tool/prompt_lib/42/101", withTokens},
		"a key the engine never sends":       {&callback, "/test_tool/prompt_lib/42/101", withUnknown},
		"toolkit_config beyond toolkit_id":   {&callback, "/test_tool/prompt_lib/42/101", configExtra},
		"toolkit_config that is no object":   {&callback, "/test_tool/prompt_lib/42/101", configNotObject},
		"a body that is an array":            {&callback, "/test_tool/prompt_lib/42/101", `[]`},
		"a body with no tool_name":           {&callback, "/test_tool/prompt_lib/42/101", `{"request_id":"x"}`},
		"a body that is not JSON":            {&callback, "/test_tool/prompt_lib/42/101", `{"tool_name":`},
		"no principal":                       {nil, "/test_tool/prompt_lib/42/101", engineBody},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			response := serve(t, gate(&fakeGrants{}, "tool.execute"), tc.path, tc.body, tc.user)
			if response.Code != http.StatusForbidden || response.Body.String() != "denied by tool.patch" {
				t.Fatalf("want the patch gate's refusal, got %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

// The expiry, the owner and the grant's tool are the store's to decide; the
// gate passes the AUTHENTICATING token's id and fails closed on a lookup
// error.
func TestAnExpiredOrUnrecordedGrantAndALookupFailureTakeThePatchGate(t *testing.T) {
	user := callbackUser()
	user.TokenID = "902" // a token the store does not admit (expired, another tool, a PAT)
	response := serve(t, gate(&fakeGrants{}, "tool.execute"), "/test_tool/prompt_lib/42/101", engineBody, &user)
	if response.Code != http.StatusForbidden || response.Body.String() != "denied by tool.patch" {
		t.Fatalf("an unrecorded token was admitted: %d %s", response.Code, response.Body.String())
	}
	failing := &fakeGrants{err: errors.New("database down")}
	user = callbackUser()
	response = serve(t, gate(failing, "tool.execute"), "/test_tool/prompt_lib/42/101", engineBody, &user)
	if response.Code != http.StatusForbidden || failing.lookups != 1 {
		t.Fatalf("a failed lookup must fall back to patch: %d (lookups %d)", response.Code, failing.lookups)
	}
}

// A caller WITH patch is unaffected by any of this: the patch gate admits it
// and the body arrives intact.
func TestACallerWithPatchIsUnchanged(t *testing.T) {
	pat := auth.User{ID: "11", UserID: "11", TokenID: "901", AuthType: "token"}
	write := strings.Replace(engineBody, `"get_issue"`, `"create_issue"`, 1)
	response := serve(t, gate(&fakeGrants{}, "tool.patch"), "/test_tool/prompt_lib/42/101", write, &pat)
	if response.Code != http.StatusOK || response.Header().Get("X-Gate") != "tool.patch" ||
		response.Body.String() != write {
		t.Fatalf("a patch holder was disturbed: %d %q %s",
			response.Code, response.Header().Get("X-Gate"), response.Body.String())
	}
}

func TestReadOnlySourceTool(t *testing.T) {
	for name, want := range map[string]bool{
		"get_issue": true, "list_files": true, "search_code": true, "Read_File": true,
		"create_issue": false, "update_file": false, "list_branches_in_repo": false,
		"get_tags": false, "run_query": false, "": false, "delete_branch": false,
	} {
		if got := material.ReadOnlySourceTool(name); got != want {
			t.Errorf("ReadOnlySourceTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestReadOnlyRulesMatchTheEngineAsset pins the Go restatement to the lists
// the Inventory engine filters its offered tools with. A rule the engine
// tightens and the platform does not would leave the server-side check
// weaker than the tool list the model is shown.
func TestReadOnlyRulesMatchTheEngineAsset(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..", "libs", "rust", "inventory-core", "assets", "python_inventory.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("the engine asset is not in this checkout: %s", path)
	}
	if err != nil {
		t.Fatalf("read the engine asset: %v", err)
	}
	var document struct {
		Investigate struct {
			Prefixes []string `json:"read_only_prefixes"`
			Patterns []string `json:"write_operation_patterns"`
		} `json:"investigate"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse the engine asset: %v", err)
	}
	asset := document.Investigate
	if len(asset.Prefixes) == 0 || len(asset.Patterns) == 0 {
		t.Fatal("the engine asset no longer carries the read-only rule; this pin measures nothing")
	}
	if !slices.Equal(asset.Prefixes, material.ReadOnlyToolPrefixes) {
		t.Errorf("read-only prefixes drifted:\n engine %v\n go     %v", asset.Prefixes, material.ReadOnlyToolPrefixes)
	}
	if !slices.Equal(asset.Patterns, material.WriteOperationPatterns) {
		t.Errorf("write patterns drifted:\n engine %v\n go     %v", asset.Patterns, material.WriteOperationPatterns)
	}
}

type countingToolkits struct{ gets int }

func (c *countingToolkits) Get(_ context.Context, _, id int32) (repos.CurrentToolkit, error) {
	c.gets++
	if id == 101 {
		sources := make([]any, 0, 200)
		for source := 1; source <= 200; source++ {
			sources = append(sources, float64(1000+source))
		}
		return repos.CurrentToolkit{ID: id, Type: "inventory", Settings: map[string]any{"sources": sources}}, nil
	}
	return repos.CurrentToolkit{ID: id, Type: "github"}, nil
}

type fakeMinter struct{}

func (fakeMinter) Mint(context.Context, int64, int64, string, time.Duration) (material.Grant, error) {
	return material.Grant{Bearer: "b", UUID: "u", Expires: time.Now().Add(time.Minute)}, nil
}
func (fakeMinter) Revoke(context.Context, int64, string) error { return nil }

type recordingGrants struct{ grant repos.CallbackTokenGrant }

func (r *recordingGrants) Record(_ context.Context, grant repos.CallbackTokenGrant) error {
	r.grant = grant
	return nil
}

// A sources list is bounded: a toolkit naming 200 sources costs at most the
// cap of reads (plus the invoking toolkit's own), not one per entry.
func TestAGrantReadsAtMostTheCapOfSourceToolkits(t *testing.T) {
	toolkits := &countingToolkits{}
	grants := &recordingGrants{}
	rewriter := material.SourceRewriter{
		Provider: "inventory", Minter: fakeMinter{}, CallbackBase: "http://cb", Lifetime: time.Minute,
		OwnerField: "application_id", Grants: grants,
		Expander: material.Expander{
			Toolkits: toolkits, SourcesField: "sources", Allowed: []string{"github"},
			Kinds: map[string]material.Kind{"github": {}},
		},
	}
	rewrite := rewriter.GrantRewriteFor("investigate", "investigate")("", "investigate")
	body := `{"configuration":{"application_id":101,"parameters":{}}}`
	if _, _, err := rewrite(context.Background(), strings.NewReader(body), 42, 11); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if toolkits.gets > 65 {
		t.Fatalf("%d toolkit reads for 200 listed sources; want at most 65", toolkits.gets)
	}
	if got := len(grants.grant.SourceToolkitIDs); got != 64 {
		t.Fatalf("granted %d sources, want the first 64", got)
	}
}
