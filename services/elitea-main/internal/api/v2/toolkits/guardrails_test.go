package toolkits_test

// What the guardrails policy does to each toolkit surface, and — as importantly
// — what it deliberately does not do.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

type guardrailSourceStub struct {
	policy guardrails.Policy
	err    error
	calls  int
}

func (stub *guardrailSourceStub) GuardrailPolicy(context.Context) (guardrails.Policy, error) {
	stub.calls++
	if stub.err != nil {
		return guardrails.Policy{}, stub.err
	}
	return stub.policy, nil
}

func guardrailRouter(repo toolkits.Repository, source toolkits.GuardrailPolicySource) *chi.Mux {
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithGuardrails(source))
	router := chi.NewRouter()
	router.Get("/toolkit_types/prompt_lib/{projectID}", handler.ListTypes)
	router.Get("/toolkits/prompt_lib/{projectID}", handler.ListTypeSchemas)
	router.Get("/toolkit_available_tools/prompt_lib/{projectID}/{toolkitID}", handler.AvailableTools)
	router.Post("/toolkit_discover_tools/prompt_lib/{projectID}/{toolkitType}", handler.DiscoverTools)
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Put("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	router.Post("/fork_toolkit/prompt_lib/{projectID}", handler.ForkToolkit)
	return router
}

func guardrailDo(t *testing.T, router *chi.Mux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestListTypesDropsBlockedToolkitTypes(t *testing.T) {
	// The tenant read contributes types too, so the filter has to run over the
	// MERGED list — a blocked type must not re-enter through the database after
	// being removed from the static one.
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"Data-Source", "github_loader"},
	})}
	router := guardrailRouter(&mockRepo{types: []string{"datasource", "sharepoint"}}, source)

	recorder := guardrailDo(t, router, http.MethodGet, "/toolkit_types/prompt_lib/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body struct {
		Rows  []string `json:"rows"`
		Total int      `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, row := range body.Rows {
		if row == "datasource" || row == "github_loader" {
			t.Fatalf("blocked type %q survived: %v", row, body.Rows)
		}
	}
	if body.Total != len(body.Rows) {
		t.Fatalf("total=%d rows=%d", body.Total, len(body.Rows))
	}
	if len(body.Rows) == 0 {
		t.Fatal("the unblocked types must still be served")
	}
}

func TestListTypeSchemasDropsBlockedTypesAndTools(t *testing.T) {
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"artifact"},
		// Configured in a different naming style than the schema declares, to
		// prove the catalogue filter goes through the canonical matcher.
		BlockedTools: map[string][]string{"GitHub": {"Create-Issue"}},
	})}
	router := guardrailRouter(&mockRepo{}, source)

	recorder := guardrailDo(t, router, http.MethodGet, "/toolkits/prompt_lib/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var catalogue map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalogue); err != nil {
		t.Fatal(err)
	}

	if _, present := catalogue["artifact"]; present {
		t.Fatal("a blocked toolkit type must not appear in the catalogue")
	}
	github, present := catalogue["github"]
	if !present {
		t.Fatal("an unblocked type must still be served")
	}
	tools := githubArgsSchemas(t, github)
	if _, present := tools["create_issue"]; present {
		t.Fatal("a blocked tool must not appear in its type's args_schemas")
	}
	if _, present := tools["get_issue"]; !present {
		t.Fatal("the type's other tools must survive")
	}
}

func githubArgsSchemas(t *testing.T, typeSchema map[string]any) map[string]any {
	t.Helper()
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("no properties: %v", typeSchema)
	}
	selected, ok := properties["selected_tools"].(map[string]any)
	if !ok {
		t.Fatalf("no selected_tools: %v", properties)
	}
	args, ok := selected["args_schemas"].(map[string]any)
	if !ok {
		t.Fatalf("no args_schemas: %v", selected)
	}
	return args
}

func TestDiscoverToolsRefusesABlockedTypeRatherThanEmptyingIt(t *testing.T) {
	// "this type is blocked" and "this project has no toolkits of this type"
	// are different facts, and the type is in the URL, so the first is
	// answerable.
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"shell"},
	})}
	router := guardrailRouter(&mockRepo{tools: []toolkits.Tool{{ID: "1", Name: "s", Type: "shell"}}}, source)

	recorder := guardrailDo(t, router, http.MethodPost, "/toolkit_discover_tools/prompt_lib/1/shell", nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("shell")) {
		t.Fatalf("the refusal must name the blocked type: %s", recorder.Body.String())
	}
}

func TestAvailableToolsDropsRowsOfABlockedType(t *testing.T) {
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"shell"},
	})}
	router := guardrailRouter(&mockRepo{tools: []toolkits.Tool{
		{ID: "1", Name: "danger", Type: "shell"},
		{ID: "2", Name: "docs", Type: "confluence"},
	}}, source)

	recorder := guardrailDo(t, router, http.MethodGet, "/toolkit_available_tools/prompt_lib/1/7", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body struct {
		Tools []toolkits.Tool `json:"tools"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Type != "confluence" {
		t.Fatalf("tools=%v", body.Tools)
	}
	if body.Total != 1 {
		t.Fatalf("total=%d must agree with the filtered list", body.Total)
	}
}

func TestWritePathsRefuseABlockedToolkitType(t *testing.T) {
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"shell"},
	})}
	router := guardrailRouter(&mockRepo{}, source)

	for _, testCase := range []struct {
		name   string
		method string
		path   string
	}{
		{"create", http.MethodPost, "/tools/prompt_lib/1"},
		{"update", http.MethodPut, "/tool/prompt_lib/1/7"},
		{"fork", http.MethodPost, "/fork_toolkit/prompt_lib/1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := guardrailDo(t, router, testCase.method, testCase.path,
				map[string]any{"type": "Shell", "name": "x"})
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestUpdateWithoutATypeIsNotRefused(t *testing.T) {
	// An update that does not restate the type cannot introduce a blocked one,
	// and refusing it would make a toolkit of a newly-blocked type impossible to
	// edit — including impossible to point somewhere harmless before deleting.
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"shell"},
	})}
	router := guardrailRouter(&mockRepo{}, source)

	recorder := guardrailDo(t, router, http.MethodPut, "/tool/prompt_lib/1/7",
		map[string]any{"name": "renamed"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAPolicyReadFailureServesTheSurfaceUnfiltered(t *testing.T) {
	// The API surfaces degrade permissively: refusing to list toolkit types
	// because one configuration row could not be read would take the
	// create-toolkit form down to enforce a policy that is usually empty. The
	// agent freeze makes the opposite choice, which is why
	// platformconfig.LoadGuardrails returns the error instead of choosing.
	source := &guardrailSourceStub{err: errors.New("pool is gone")}
	router := guardrailRouter(&mockRepo{types: []string{"datasource"}}, source)

	recorder := guardrailDo(t, router, http.MethodGet, "/toolkit_types/prompt_lib/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if source.calls == 0 {
		t.Fatal("the policy must have been consulted")
	}
}

func TestAnUnwiredSourceBlocksNothing(t *testing.T) {
	// A Handler with no source is one no composition root gave a database to.
	// It must behave exactly as it did before guardrails existed.
	router := chi.NewRouter()
	handler := toolkits.NewHandlerWithRepo(&mockRepo{types: []string{"datasource"}})
	router.Get("/toolkit_types/prompt_lib/{projectID}", handler.ListTypes)

	recorder := guardrailDo(t, router, http.MethodGet, "/toolkit_types/prompt_lib/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("datasource")) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}

// TestListTypeSchemasBlocksAToolOnAnSDKBackedType is the composition-root twin
// of TestListTypeSchemasDropsBlockedTypesAndTools above, and the two are not
// redundant.
//
// That test composes a Handler with NO argument-schema source, so the catalogue
// falls back to handler.go's hand-written type overrides, whose args_schemas are
// `map[string]any` literals. This one composes the source the deployment
// composes — the pinned SDK snapshot — whose entries carry
// `map[string]map[string]any`. `withoutBlockedTools` used to assert the first
// type only, so it filtered the eight stubs and silently kept every tool of the
// 45 real types. The package's own suite was green while `github`'s
// `apply_git_patch` survived being blocked on a live deployment.
//
// The tool is DERIVED from the served catalogue rather than named, so a future
// SDK rename fails on the assertion above rather than as "guardrails broke".
func TestListTypeSchemasBlocksAToolOnAnSDKBackedType(t *testing.T) {
	t.Parallel()

	unfiltered := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))
	tools := sortedArgumentSchemaNames(t, unfiltered, "github")
	if len(tools) < 2 {
		t.Fatalf("the github type schema offers %d tools; this test needs a blocked one and a survivor", len(tools))
	}
	target, survivor := tools[0], tools[1]

	// Blocked in a DIFFERENT case and a different naming style than the schema
	// declares it, which is the property the admin form's free-text editor and
	// the SDK's canonical matcher exist for.
	source := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedTools: map[string][]string{"GitHub": {strings.ToUpper(target)}},
	})}
	filtered := getToolkitTypeCatalogue(t,
		toolkits.WithArgumentSchemas(pinnedSnapshot(t)),
		toolkits.WithGuardrails(source),
	)

	if _, present := filtered["github"]; !present {
		t.Fatal("blocking a TOOL must not remove its TYPE from the catalogue")
	}
	names := sortedArgumentSchemaNames(t, filtered, "github")
	for _, name := range names {
		if name == target {
			t.Fatalf("blocking %q left %q in the github args_schemas", strings.ToUpper(target), target)
		}
	}
	if !containsName(names, survivor) {
		t.Fatalf("blocking one tool removed %q as well; have %v", survivor, names)
	}
}

func sortedArgumentSchemaNames(t *testing.T, body map[string]any, toolkitType string) []string {
	t.Helper()
	selected := selectedToolsSchema(t, body, toolkitType)
	schemas, ok := selected["args_schemas"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no args_schemas object: %#v", toolkitType, selected)
	}
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
