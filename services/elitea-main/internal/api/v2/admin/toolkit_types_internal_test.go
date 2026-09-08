package admin

// The five routes of `Admin › Toolkits`, measured without a database.
//
// Every write goes through a recording store, so a REFUSED write can be shown
// to have stored nothing. A refusal that still wrote is how an operator ends up
// believing a type is off while the catalogue still serves it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
)

/* ── a recording store ──────────────────────────────────────────────────── */

type recordingPolicyStore struct {
	policies []toolkitcatalogue.Policy
	grants   []toolkitcatalogue.Grant

	savedPolicies   []toolkitcatalogue.Policy
	deletedPolicies []string
	savedGrants     []toolkitcatalogue.Grant
	deletedGrants   []string

	listErr  error
	writeErr error
	// grantMissingPolicy makes SaveGrant report the foreign-key refusal the
	// database would report.
	grantMissingPolicy bool
}

func (s *recordingPolicyStore) ListPolicies(context.Context) ([]toolkitcatalogue.Policy, error) {
	return s.policies, s.listErr
}

func (s *recordingPolicyStore) ListGrants(context.Context) ([]toolkitcatalogue.Grant, error) {
	return s.grants, s.listErr
}

func (s *recordingPolicyStore) SavePolicy(
	_ context.Context, policy toolkitcatalogue.Policy,
) (toolkitcatalogue.Policy, error) {
	if s.writeErr != nil {
		return toolkitcatalogue.Policy{}, s.writeErr
	}
	if _, err := toolkitcatalogue.ValidateToolkitType(policy.ToolkitType); err != nil {
		return toolkitcatalogue.Policy{}, err
	}
	if _, err := toolkitcatalogue.ParseAvailability(string(policy.Availability)); err != nil {
		return toolkitcatalogue.Policy{}, toolkitcatalogue.PolicyValidationError{Message: err.Error()}
	}
	reason, err := toolkitcatalogue.ValidateReason(policy.Reason)
	if err != nil {
		return toolkitcatalogue.Policy{}, err
	}
	policy.Reason = reason
	policy.DecidedAt = time.Unix(0, 0).UTC()
	s.savedPolicies = append(s.savedPolicies, policy)
	return policy, nil
}

func (s *recordingPolicyStore) DeletePolicy(_ context.Context, toolkitType string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if _, err := toolkitcatalogue.ValidateToolkitType(toolkitType); err != nil {
		return err
	}
	s.deletedPolicies = append(s.deletedPolicies, toolkitType)
	return nil
}

func (s *recordingPolicyStore) SaveGrant(
	_ context.Context, grant toolkitcatalogue.Grant,
) (toolkitcatalogue.Grant, error) {
	if s.grantMissingPolicy {
		return toolkitcatalogue.Grant{}, toolkitcatalogue.ErrPolicyNotFound
	}
	if s.writeErr != nil {
		return toolkitcatalogue.Grant{}, s.writeErr
	}
	if _, err := toolkitcatalogue.ParseGrantAvailability(string(grant.Availability)); err != nil {
		return toolkitcatalogue.Grant{}, toolkitcatalogue.PolicyValidationError{Message: err.Error()}
	}
	reason, err := toolkitcatalogue.ValidateReason(grant.Reason)
	if err != nil {
		return toolkitcatalogue.Grant{}, err
	}
	grant.Reason = reason
	grant.GrantedAt = time.Unix(0, 0).UTC()
	s.savedGrants = append(s.savedGrants, grant)
	return grant, nil
}

func (s *recordingPolicyStore) DeleteGrant(_ context.Context, toolkitType string, projectID int64) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if projectID == 4242 {
		return toolkitcatalogue.ErrPolicyNotFound
	}
	s.deletedGrants = append(s.deletedGrants, toolkitType)
	return nil
}

/* ── a registry ─────────────────────────────────────────────────────────── */

type fakeRegistry struct{ types []string }

func (r fakeRegistry) ToolkitTypes() []string { return r.types }

func (r fakeRegistry) ToolkitToolNames(string) ([]string, bool) { return nil, false }

/* ── request helpers ────────────────────────────────────────────────────── */

func toolkitTypeRequest(method, path, body string, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	for key, value := range params {
		routeContext.URLParams.Add(key, value)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return body
}

/* ── unwired ────────────────────────────────────────────────────────────── */

// An unwired store answers 503 on every route and writes nothing. Never an
// empty list: "this platform offers no toolkit type" is never true, and
// rendering it would tell the operator their platform is broken.
func TestToolkitTypeRoutesRefuseWhenUnwired(t *testing.T) {
	handler := NewHandler(nil)

	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"list":   handler.ToolkitTypeList,
		"save":   handler.ToolkitTypeSave,
		"grant":  handler.ToolkitTypeGrantSave,
		"revoke": handler.ToolkitTypeGrantDelete,
		"bulk":   handler.ToolkitTypeBulk,
	} {
		recorder := httptest.NewRecorder()
		call(recorder, toolkitTypeRequest(http.MethodPut, "/toolkit_types/administration/github",
			`{"availability":"disabled","reason":"x"}`,
			map[string]string{"type": "github", "projectID": "1"}))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "route %s", name)
		require.Contains(t, recorder.Body.String(), "not configured", "route %s", name)
	}
}

// A nil store passed to the option must leave the handler unwired rather than
// boxing a nil into a non-nil interface — the typed-nil shape this repository
// has shipped before.
func TestWithToolkitTypePolicyIgnoresNil(t *testing.T) {
	handler := NewHandler(nil, WithToolkitTypePolicy(nil))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeList(recorder, toolkitTypeRequest(http.MethodGet, "/toolkit_types/administration", "", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

/* ── the listing ────────────────────────────────────────────────────────── */

func TestToolkitTypeListJoinsRegistryPolicyAndCapability(t *testing.T) {
	store := &recordingPolicyStore{
		policies: []toolkitcatalogue.Policy{{
			ToolkitType: "sql", Availability: toolkitcatalogue.AvailabilityRestricted,
			Reason: "per contract", DecidedBy: "operator@example.com", DecidedAt: time.Unix(0, 0).UTC(),
		}},
		grants: []toolkitcatalogue.Grant{{
			ToolkitType: "sql", ProjectID: 7, Availability: toolkitcatalogue.AvailabilityEnabled,
			Reason: "contract 4471", GrantedBy: "operator@example.com", GrantedAt: time.Unix(0, 0).UTC(),
		}},
	}
	handler := NewHandler(nil,
		WithToolkitTypePolicy(store),
		WithToolkitRegistry(fakeRegistry{types: []string{"github", "sql", "pptx"}}),
	)

	recorder := httptest.NewRecorder()
	handler.ToolkitTypeList(recorder,
		toolkitTypeRequest(http.MethodGet, "/toolkit_types/administration", "", nil))
	require.Equal(t, http.StatusOK, recorder.Code)

	body := decodeBody(t, recorder)
	require.Equal(t, true, body["registry_available"])
	require.NotEmpty(t, body["categories"])
	types, ok := body["types"].([]any)
	require.True(t, ok)
	require.Len(t, types, 3)

	byType := map[string]map[string]any{}
	for _, entry := range types {
		row := entry.(map[string]any)
		byType[row["type"].(string)] = row
	}

	// A type nobody decided about reads as `default` and stays AVAILABLE, with
	// no invented reason.
	github := byType["github"]
	require.Equal(t, "default", github["availability"])
	require.Equal(t, true, github["available"])
	require.Equal(t, "", github["reason"])
	require.Equal(t, "GitHub", github["label"])
	require.Equal(t, "code_repositories", github["category"])
	require.Equal(t, true, github["registered"])

	// The decided type carries its provenance and its exception.
	sql := byType["sql"]
	require.Equal(t, "restricted", sql["availability"])
	require.Equal(t, false, sql["available"])
	require.Equal(t, "per contract", sql["reason"])
	require.Equal(t, "operator@example.com", sql["decided_by"])
	require.Equal(t, "deployment", sql["source"])
	grants := sql["project_grants"].([]any)
	require.Len(t, grants, 1)
	require.EqualValues(t, 7, grants[0].(map[string]any)["project_id"])

	// The capability verdict travels with the row, with the sentence behind it.
	capability := byType["pptx"]["capability"].(map[string]any)
	require.Equal(t, "unverified", capability["verdict"])
	require.NotEmpty(t, capability["reason"])
	require.Equal(t, false, capability["python"])
	require.Equal(t, false, capability["rust"])
}

// A policy row for a type the registry no longer enumerates must still be
// listed. A decision the operator cannot see is a decision they cannot reverse,
// and the row still filters the served catalogue.
func TestToolkitTypeListKeepsDecisionsForUnregisteredTypes(t *testing.T) {
	store := &recordingPolicyStore{policies: []toolkitcatalogue.Policy{{
		ToolkitType: "retired_type", Availability: toolkitcatalogue.AvailabilityDisabled,
		Reason: "withdrawn", DecidedBy: "operator@example.com",
	}}}
	handler := NewHandler(nil,
		WithToolkitTypePolicy(store), WithToolkitRegistry(fakeRegistry{types: []string{"github"}}))

	recorder := httptest.NewRecorder()
	handler.ToolkitTypeList(recorder,
		toolkitTypeRequest(http.MethodGet, "/toolkit_types/administration", "", nil))
	require.Equal(t, http.StatusOK, recorder.Code)

	body := decodeBody(t, recorder)
	types := body["types"].([]any)
	require.Len(t, types, 2)
	for _, entry := range types {
		row := entry.(map[string]any)
		if row["type"] == "retired_type" {
			require.Equal(t, false, row["registered"])
			return
		}
	}
	t.Fatal("the decision for an unregistered type vanished from the listing")
}

// An unwired registry must not read as "no toolkit types". The listing says so
// rather than answering an empty grid.
func TestToolkitTypeListReportsAnUnwiredRegistry(t *testing.T) {
	handler := NewHandler(nil, WithToolkitTypePolicy(&recordingPolicyStore{}))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeList(recorder,
		toolkitTypeRequest(http.MethodGet, "/toolkit_types/administration", "", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, false, decodeBody(t, recorder)["registry_available"])
}

func TestToolkitTypeListReportsAReadFailure(t *testing.T) {
	handler := NewHandler(nil,
		WithToolkitTypePolicy(&recordingPolicyStore{listErr: errors.New("pool is closed")}))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeList(recorder,
		toolkitTypeRequest(http.MethodGet, "/toolkit_types/administration", "", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	// The database's own words must not cross the boundary.
	require.NotContains(t, recorder.Body.String(), "pool is closed")
}

/* ── the decision ───────────────────────────────────────────────────────── */

func TestToolkitTypeSaveRecordsTheDecisionAndTheDecider(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))

	recorder := httptest.NewRecorder()
	handler.ToolkitTypeSave(recorder, toolkitTypeRequest(
		http.MethodPut, "/toolkit_types/administration/sql",
		`{"availability":"restricted","reason":"offered per contract"}`,
		map[string]string{"type": "sql"}))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, store.savedPolicies, 1)
	require.Equal(t, "sql", store.savedPolicies[0].ToolkitType)
	require.Equal(t, toolkitcatalogue.AvailabilityRestricted, store.savedPolicies[0].Availability)
	require.Equal(t, "offered per contract", store.savedPolicies[0].Reason)
	require.NotEmpty(t, store.savedPolicies[0].DecidedBy,
		"an unattributed decision is one nobody can ask about")
}

// `default` is the revert, and it DELETES rather than storing the word.
func TestToolkitTypeSaveDefaultReverts(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))

	recorder := httptest.NewRecorder()
	handler.ToolkitTypeSave(recorder, toolkitTypeRequest(
		http.MethodPut, "/toolkit_types/administration/sql",
		`{"availability":"default"}`, map[string]string{"type": "sql"}))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"sql"}, store.deletedPolicies)
	require.Empty(t, store.savedPolicies)
	require.Equal(t, "default", decodeBody(t, recorder)["availability"])
}

// The refusals, and the proof that a refused write stored nothing.
func TestToolkitTypeSaveRefusalsStoreNothing(t *testing.T) {
	for name, testCase := range map[string]struct {
		body       string
		wantStatus int
	}{
		"no reason":            {`{"availability":"disabled","reason":"  "}`, http.StatusBadRequest},
		"unknown availability": {`{"availability":"maybe","reason":"x"}`, http.StatusBadRequest},
		"invalid json":         {`{`, http.StatusBadRequest},
	} {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			store := &recordingPolicyStore{}
			handler := NewHandler(nil, WithToolkitTypePolicy(store))
			recorder := httptest.NewRecorder()
			handler.ToolkitTypeSave(recorder, toolkitTypeRequest(
				http.MethodPut, "/toolkit_types/administration/sql", testCase.body,
				map[string]string{"type": "sql"}))
			require.Equal(t, testCase.wantStatus, recorder.Code)
			require.Empty(t, store.savedPolicies, "a refused write stored a decision")
			require.Empty(t, store.deletedPolicies)
		})
	}
}

func TestToolkitTypeSaveReportsAWriteFailureAsUnavailable(t *testing.T) {
	handler := NewHandler(nil,
		WithToolkitTypePolicy(&recordingPolicyStore{writeErr: errors.New("relation does not exist")}))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeSave(recorder, toolkitTypeRequest(
		http.MethodPut, "/toolkit_types/administration/sql",
		`{"availability":"disabled","reason":"x"}`, map[string]string{"type": "sql"}))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "relation does not exist")
}

/* ── grants ─────────────────────────────────────────────────────────────── */

func TestToolkitTypeGrantAndRevoke(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))

	recorder := httptest.NewRecorder()
	handler.ToolkitTypeGrantSave(recorder, toolkitTypeRequest(
		http.MethodPut, "/toolkit_types/administration/sql/projects/7",
		`{"availability":"enabled","reason":"contract 4471"}`,
		map[string]string{"type": "sql", "projectID": "7"}))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, store.savedGrants, 1)
	require.EqualValues(t, 7, store.savedGrants[0].ProjectID)

	recorder = httptest.NewRecorder()
	handler.ToolkitTypeGrantDelete(recorder, toolkitTypeRequest(
		http.MethodDelete, "/toolkit_types/administration/sql/projects/7", "",
		map[string]string{"type": "sql", "projectID": "7"}))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"sql"}, store.deletedGrants)
}

// 409, not 404: the project and the type both exist. What is missing is the
// deployment decision the grant would be an exception to.
func TestGrantWithNoDecisionAnswersConflict(t *testing.T) {
	handler := NewHandler(nil, WithToolkitTypePolicy(&recordingPolicyStore{grantMissingPolicy: true}))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeGrantSave(recorder, toolkitTypeRequest(
		http.MethodPut, "/toolkit_types/administration/sql/projects/7",
		`{"availability":"enabled","reason":"x"}`,
		map[string]string{"type": "sql", "projectID": "7"}))
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), "decide about this toolkit type first")
}

func TestGrantRefusesABadProjectID(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))
	for _, projectID := range []string{"", "0", "-3", "seven"} {
		recorder := httptest.NewRecorder()
		handler.ToolkitTypeGrantSave(recorder, toolkitTypeRequest(
			http.MethodPut, "/toolkit_types/administration/sql/projects/x",
			`{"availability":"enabled","reason":"x"}`,
			map[string]string{"type": "sql", "projectID": projectID}))
		require.Equal(t, http.StatusBadRequest, recorder.Code, "project id %q", projectID)
	}
	require.Empty(t, store.savedGrants)
}

func TestRevokingAGrantThatIsNotThereAnswersNotFound(t *testing.T) {
	handler := NewHandler(nil, WithToolkitTypePolicy(&recordingPolicyStore{}))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeGrantDelete(recorder, toolkitTypeRequest(
		http.MethodDelete, "/toolkit_types/administration/sql/projects/4242", "",
		map[string]string{"type": "sql", "projectID": "4242"}))
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

/* ── bulk ───────────────────────────────────────────────────────────────── */

func TestToolkitTypeBulkAppliesEveryTypeAndReportsPartialResults(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))

	recorder := httptest.NewRecorder()
	handler.ToolkitTypeBulk(recorder, toolkitTypeRequest(
		http.MethodPost, "/toolkit_types/administration/bulk",
		`{"types":["pptx","carrier","   "],"availability":"disabled","reason":"the worker image does not carry these"}`,
		nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	body := decodeBody(t, recorder)
	require.EqualValues(t, 2, body["total"])
	require.Len(t, body["applied"], 2)
	// The failure is REPORTED, not hidden: an operator who cannot tell which of
	// fifty types landed has no way to finish the job.
	require.Len(t, body["failed"], 1)
	require.Len(t, store.savedPolicies, 2)
}

func TestToolkitTypeBulkRefusesAnEmptyOrOversizedRequest(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))

	oversized := make([]string, maxBulkTypes+1)
	for index := range oversized {
		oversized[index] = "type"
	}
	encoded, err := json.Marshal(map[string]any{
		"types": oversized, "availability": "disabled", "reason": "x",
	})
	require.NoError(t, err)

	for name, body := range map[string]string{
		"empty":     `{"types":[],"availability":"disabled","reason":"x"}`,
		"oversized": string(encoded),
		"bad json":  `{`,
	} {
		recorder := httptest.NewRecorder()
		handler.ToolkitTypeBulk(recorder, toolkitTypeRequest(
			http.MethodPost, "/toolkit_types/administration/bulk", body, nil))
		require.Equal(t, http.StatusBadRequest, recorder.Code, "case %s", name)
	}
	require.Empty(t, store.savedPolicies)
}

// Nothing landed must not answer 200. A page that shows a spinner and then no
// change is the "looks fine, is wrong" outcome this surface exists to remove.
func TestToolkitTypeBulkAnswersBadRequestWhenNothingLanded(t *testing.T) {
	handler := NewHandler(nil, WithToolkitTypePolicy(&recordingPolicyStore{}))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeBulk(recorder, toolkitTypeRequest(
		http.MethodPost, "/toolkit_types/administration/bulk",
		`{"types":["pptx"],"availability":"disabled","reason":"  "}`, nil))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Len(t, decodeBody(t, recorder)["failed"], 1)
}

func TestToolkitTypeBulkCanRevert(t *testing.T) {
	store := &recordingPolicyStore{}
	handler := NewHandler(nil, WithToolkitTypePolicy(store))
	recorder := httptest.NewRecorder()
	handler.ToolkitTypeBulk(recorder, toolkitTypeRequest(
		http.MethodPost, "/toolkit_types/administration/bulk",
		`{"types":["pptx","carrier"],"availability":"default"}`, nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"pptx", "carrier"}, store.deletedPolicies)
	require.Empty(t, store.savedPolicies)
}

/* ── the permission string ──────────────────────────────────────────────── */

// The constant the router gates on. Pinned so a rename cannot silently leave
// the routes gated on a string migration 0114 does not grant — which on a clean
// database is 403 for every caller, and reads as a broken page.
func TestToolkitTypeManagePermissionIsTheGrantedString(t *testing.T) {
	require.Equal(t, "toolkit_catalogue.type.manage", ToolkitTypeManagePermission)
}
