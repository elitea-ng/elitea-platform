package toolkits_test

// The operator's toolkit TYPE policy, applied to the two served surfaces.
//
// The composition contract in type_policy.go states three rules. Each of them
// has a test here, and each would pass a positive-only suite while broken, so
// every case measures both directions.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

type policySourceStub struct {
	policies []toolkitcatalogue.Policy
	grants   []toolkitcatalogue.Grant
	err      error
	// projects records every project id the catalogue asked about, so a test
	// can show the filter was built for the project in the URL.
	projects []int64
}

func (stub *policySourceStub) ProjectFilter(
	_ context.Context, projectID int64,
) (toolkitcatalogue.Filter, error) {
	stub.projects = append(stub.projects, projectID)
	if stub.err != nil {
		return toolkitcatalogue.EmptyFilter(), stub.err
	}
	return toolkitcatalogue.BuildFilter(projectID, stub.policies, stub.grants), nil
}

func policyRouter(
	repo toolkits.Repository, options ...toolkits.Option,
) *chi.Mux {
	handler := toolkits.NewHandlerWithRepo(repo, options...)
	router := chi.NewRouter()
	router.Get("/toolkit_types/prompt_lib/{projectID}", handler.ListTypes)
	router.Get("/toolkits/prompt_lib/{projectID}", handler.ListTypeSchemas)
	return router
}

func catalogueKeys(t *testing.T, router *chi.Mux, path string) map[string]bool {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for key := range body {
		keys[key] = true
	}
	return keys
}

func typeList(t *testing.T, router *chi.Mux, path string) []string {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Rows []string `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Rows
}

func decision(toolkitType string, availability toolkitcatalogue.Availability) toolkitcatalogue.Policy {
	return toolkitcatalogue.Policy{
		ToolkitType: toolkitType, Availability: availability,
		Reason: "a recorded reason", DecidedBy: "operator@example.com",
	}
}

// RULE 3, and the headline. An unwired policy source serves everything, which
// is what every deployment did before this file existed.
func TestAnUnwiredPolicyServesEveryType(t *testing.T) {
	t.Parallel()

	router := policyRouter(&mockRepo{})
	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/1")
	if len(keys) == 0 {
		t.Fatal("the catalogue served nothing with no policy source wired")
	}
	if !keys["github"] {
		t.Fatalf("github vanished from an unfiltered catalogue: %v", keys)
	}
}

// A source that CANNOT ANSWER serves everything too, and does not fail the
// request. Refusing to serve the toolkit catalogue because one small table was
// unreadable would take the create-toolkit form down to enforce a policy that
// is empty on most deployments.
func TestAnUnreadablePolicyServesEveryType(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{err: errors.New("pool is closed")}
	router := policyRouter(&mockRepo{}, toolkits.WithTypePolicy(source))

	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/1")
	if !keys["github"] {
		t.Fatalf("an unreadable policy withheld a type: %v", keys)
	}
	if len(source.projects) == 0 {
		t.Fatal("the catalogue never consulted the policy source")
	}
}

// An empty policy table serves everything. The absence rule, measured at the
// HTTP surface rather than only in the resolver.
func TestNoPolicyRowsServesEveryType(t *testing.T) {
	t.Parallel()

	router := policyRouter(&mockRepo{}, toolkits.WithTypePolicy(&policySourceStub{}))
	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/1")
	if !keys["github"] || !keys["artifact"] {
		t.Fatalf("an empty policy table withheld a type: %v", keys)
	}
}

func TestDisabledTypesLeaveBothServedSurfaces(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{policies: []toolkitcatalogue.Policy{
		decision("github", toolkitcatalogue.AvailabilityDisabled),
		decision("datasource", toolkitcatalogue.AvailabilityDisabled),
	}}
	router := policyRouter(&mockRepo{types: []string{"datasource"}},
		toolkits.WithTypePolicy(source))

	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/1")
	if keys["github"] {
		t.Fatalf("a disabled type stayed in the schema catalogue: %v", keys)
	}
	if !keys["artifact"] {
		t.Fatalf("disabling one type removed another: %v", keys)
	}

	// The two surfaces must agree. A type withheld from the schema catalogue
	// but still named by the type list is a tile the client offers and cannot
	// configure, which reads as a broken form rather than as a policy.
	for _, row := range typeList(t, router, "/toolkit_types/prompt_lib/1") {
		if row == "datasource" {
			t.Fatalf("a disabled type survived the type list: %v", row)
		}
	}
}

// The "grant to one project only" case, measured from both projects through the
// real HTTP path. The project id comes from the URL, so the wrong project would
// silently get the wrong catalogue.
func TestARestrictedTypeReachesOnlyTheGrantedProject(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{
		policies: []toolkitcatalogue.Policy{decision("github", toolkitcatalogue.AvailabilityRestricted)},
		grants: []toolkitcatalogue.Grant{{
			ToolkitType: "github", ProjectID: 7,
			Availability: toolkitcatalogue.AvailabilityEnabled,
			Reason:       "contract 4471", GrantedBy: "operator@example.com",
		}},
	}
	router := policyRouter(&mockRepo{}, toolkits.WithTypePolicy(source))

	if !catalogueKeys(t, router, "/toolkits/prompt_lib/7")["github"] {
		t.Fatal("the granted project did not get the restricted type")
	}
	if catalogueKeys(t, router, "/toolkits/prompt_lib/8")["github"] {
		t.Fatal("an ungranted project got a restricted type")
	}
	if len(source.projects) != 2 || source.projects[0] != 7 || source.projects[1] != 8 {
		t.Fatalf("the filter was built for %v, not for the projects in the URLs", source.projects)
	}
}

// A project id that is not a number resolves the DEFAULT catalogue rather than
// failing. The per-project exception is what needs the id, and there is no
// exception to find.
func TestANonNumericProjectStillGetsTheDeploymentDecision(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{policies: []toolkitcatalogue.Policy{
		decision("github", toolkitcatalogue.AvailabilityDisabled),
	}}
	router := policyRouter(&mockRepo{}, toolkits.WithTypePolicy(source))

	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/not-a-number")
	if keys["github"] {
		t.Fatalf("the deployment decision was skipped for an unparseable project: %v", keys)
	}
	if !keys["artifact"] {
		t.Fatalf("an unparseable project lost the rest of the catalogue: %v", keys)
	}
}

// RULE 1, and the one this repository's guardrails file warns about by name.
// An `enabled` policy row must NEVER re-admit a type the deny-list blocks.
func TestGuardrailsStayTerminalOverAnEnabledPolicy(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{policies: []toolkitcatalogue.Policy{
		decision("artifact", toolkitcatalogue.AvailabilityEnabled),
	}}
	guardrailSource := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"artifact"},
	})}
	router := policyRouter(&mockRepo{},
		toolkits.WithTypePolicy(source), toolkits.WithGuardrails(guardrailSource))

	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/1")
	if keys["artifact"] {
		t.Fatalf("an enabled policy row re-admitted a guardrail-blocked type: %v", keys)
	}
	if !keys["github"] {
		t.Fatalf("the unblocked types must still be served: %v", keys)
	}

	// And the same on the bare type list.
	for _, row := range typeList(t, router, "/toolkit_types/prompt_lib/1") {
		if row == "artifact" {
			t.Fatal("an enabled policy row re-admitted a blocked type on the type list")
		}
	}
}

// The two filters compose: a type either of them removes is gone.
func TestPolicyAndGuardrailsBothSubtract(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{policies: []toolkitcatalogue.Policy{
		decision("github", toolkitcatalogue.AvailabilityDisabled),
	}}
	guardrailSource := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"artifact"},
	})}
	router := policyRouter(&mockRepo{},
		toolkits.WithTypePolicy(source), toolkits.WithGuardrails(guardrailSource))

	keys := catalogueKeys(t, router, "/toolkits/prompt_lib/1")
	if keys["github"] || keys["artifact"] {
		t.Fatalf("a filtered type survived: %v", keys)
	}
	if len(keys) == 0 {
		t.Fatal("the two filters removed the whole catalogue")
	}
}

// The package-level settings map is shared by every request. A filter that
// deleted from it would remove a toolkit type from every LATER request on the
// process — the exact fault withArgumentSchemas rebuilds every node to avoid.
func TestFilteringOneRequestDoesNotChangeTheNext(t *testing.T) {
	t.Parallel()

	source := &policySourceStub{policies: []toolkitcatalogue.Policy{
		decision("github", toolkitcatalogue.AvailabilityRestricted),
	}}
	router := policyRouter(&mockRepo{}, toolkits.WithTypePolicy(source))

	if catalogueKeys(t, router, "/toolkits/prompt_lib/8")["github"] {
		t.Fatal("the ungranted project got a restricted type")
	}

	// A second handler with NO policy must see the full catalogue. If the first
	// request had mutated the shared map, github would be gone here.
	plain := policyRouter(&mockRepo{})
	if !catalogueKeys(t, plain, "/toolkits/prompt_lib/8")["github"] {
		t.Fatal("a filtered request mutated the package-level catalogue")
	}
}
