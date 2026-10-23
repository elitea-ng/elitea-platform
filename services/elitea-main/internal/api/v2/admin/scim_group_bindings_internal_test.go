package admin

// What these tests hold in place on the group-binding surface.
//
// A binding decides which project an identity provider can put people into, so
// the failures that matter are the quiet ones:
//
//   - An UNWIRED surface answering an empty list. "No group is bound" is a
//     sentence an operator acts on; they would author bindings that already
//     exist, or conclude the feature is off while pushes are landing.
//   - A binding accepted with a project or a role that does not exist. It would
//     be refused later, during a push, in a log nobody reads.
//   - A refusal that does not name what was wrong.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

type stubBindingStore struct {
	groups  []scimdirectory.Group
	created []scimdirectory.Group
	deleted []int64
	roles   []string
	err     error
	// listedAt records the (startIndex, count) each listing asked for, so a
	// test can assert the page a request produced rather than the rows a stub
	// chose to return.
	listedAt [][2]int
}

func (s *stubBindingStore) ListGroups(
	_ context.Context, _ scimdirectory.Filter, startIndex, count int,
) ([]scimdirectory.Group, int, error) {
	if s.err != nil {
		return nil, 0, s.err
	}
	s.listedAt = append(s.listedAt, [2]int{startIndex, count})
	return s.groups, len(s.groups), nil
}

func (s *stubBindingStore) ProjectRoleNames(context.Context, int) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.roles, nil
}

func (s *stubBindingStore) CreateBinding(
	_ context.Context, displayName string, projectID int, roleName string,
) (scimdirectory.Group, error) {
	if s.err != nil {
		return scimdirectory.Group{}, s.err
	}
	group := scimdirectory.Group{
		ID: 7, DisplayName: displayName, ProjectID: projectID, RoleName: roleName,
	}
	s.created = append(s.created, group)
	return group, nil
}

func (s *stubBindingStore) UpdateBinding(
	_ context.Context, id int64, displayName string, projectID int, roleName string,
) (scimdirectory.Group, error) {
	if s.err != nil {
		return scimdirectory.Group{}, s.err
	}
	return scimdirectory.Group{
		ID: id, DisplayName: displayName, ProjectID: projectID, RoleName: roleName,
	}, nil
}

func (s *stubBindingStore) DeleteGroup(_ context.Context, id int64) error {
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, id)
	return nil
}

func serveBinding(
	t *testing.T, handler *Handler, method, target, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/bindings", handler.SCIMGroupBindingList)
	router.Get("/bindings/project_roles/{projectID}", handler.SCIMGroupBindingProjectRoles)
	router.Post("/bindings", handler.SCIMGroupBindingCreate)
	router.Put("/bindings/{id}", handler.SCIMGroupBindingSave)
	router.Delete("/bindings/{id}", handler.SCIMGroupBindingDelete)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
	return recorder
}

func bindingBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return body
}

// An unwired surface must not answer "there are no bindings".
func TestAnUnwiredBindingSurfaceRefusesRatherThanReportingNoBindings(t *testing.T) {
	handler := NewHandler(nil)
	for _, call := range []struct{ method, target, body string }{
		{http.MethodGet, "/bindings", ""},
		{http.MethodPost, "/bindings", `{"display_name":"Team","project_id":1,"role_name":"editor"}`},
		{http.MethodPut, "/bindings/7", `{"display_name":"Team","project_id":1,"role_name":"editor"}`},
		{http.MethodDelete, "/bindings/7", ""},
	} {
		recorder := serveBinding(t, handler, call.method, call.target, call.body)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "%s %s", call.method, call.target)
	}
}

func TestABindingIsRefusedWhenItNamesNoProjectOrNoRole(t *testing.T) {
	store := &stubBindingStore{}
	handler := NewHandler(nil, WithSCIMGroupBindings(store))

	for _, body := range []string{
		`{"display_name":"","project_id":1,"role_name":"editor"}`,
		`{"display_name":"Team","role_name":"editor"}`,
		`{"display_name":"Team","project_id":1,"role_name":"  "}`,
	} {
		recorder := serveBinding(t, handler, http.MethodPost, "/bindings", body)
		require.Equal(t, http.StatusBadRequest, recorder.Code, body)
	}
	// None of them reached the store. A refusal that had already written is the
	// state neither screen shows.
	require.Empty(t, store.created)
}

// The store's verdicts are echoed with the value an operator has to change.
func TestTheStoresRefusalsAreNamedOnTheWire(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		err      error
		status   int
		contains string
	}{
		{"unknown project", scimdirectory.UnknownProjectError{ProjectID: 42},
			http.StatusBadRequest, "42"},
		{"missing role", scimdirectory.RoleMissingError{ProjectID: 7, RoleName: "auditor"},
			http.StatusBadRequest, "auditor"},
		{"duplicate name", scimdirectory.ErrConflict, http.StatusConflict, "already uses"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler := NewHandler(nil, WithSCIMGroupBindings(&stubBindingStore{err: testCase.err}))
			recorder := serveBinding(t, handler, http.MethodPost, "/bindings",
				`{"display_name":"Team","project_id":7,"role_name":"auditor"}`)

			require.Equal(t, testCase.status, recorder.Code)
			require.Contains(t, bindingBody(t, recorder)["error"], testCase.contains)
		})
	}
}

func TestAListingReportsWhoTheGroupGrantedAndWhoItMerelyFound(t *testing.T) {
	store := &stubBindingStore{groups: []scimdirectory.Group{{
		ID: 7, DisplayName: "Platform Team", ProjectID: 12, ProjectName: "Platform",
		RoleName: "editor",
		Members: []scimdirectory.GroupMember{
			{UserID: 1, UserName: "alice@corp.com", Granted: true},
			{UserID: 2, UserName: "bob@corp.com", Granted: false},
		},
	}}}
	handler := NewHandler(nil, WithSCIMGroupBindings(store))

	recorder := serveBinding(t, handler, http.MethodGet, "/bindings", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	bindings := bindingBody(t, recorder)["bindings"].([]any)
	require.Len(t, bindings, 1)
	binding := bindings[0].(map[string]any)
	// The id is a STRING, as it is on the SCIM resource, so the screen and the
	// identity provider address the same binding the same way.
	require.Equal(t, "7", binding["id"])
	members := binding["members"].([]any)
	require.Equal(t, true, members[0].(map[string]any)["granted"])
	require.Equal(t, false, members[1].(map[string]any)["granted"],
		"a member the group only found must be distinguishable on the screen")
}

func TestDeletingABindingReachesTheStoreAndAnswersNoContent(t *testing.T) {
	store := &stubBindingStore{}
	handler := NewHandler(nil, WithSCIMGroupBindings(store))

	recorder := serveBinding(t, handler, http.MethodDelete, "/bindings/7", "")
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, []int64{7}, store.deleted)
}

/* ── the listing is paged, not capped ──────────────────────────────────── */

// A capped listing renders its first page and reports a larger total, and every
// binding past the cap is unreachable from any screen: an operator looking for
// one concludes it does not exist and authors a duplicate, which the unique
// index then refuses for a reason they cannot see.
func TestTheListingPagesRatherThanCappingSilently(t *testing.T) {
	store := &stubBindingStore{}
	handler := NewHandler(nil, WithSCIMGroupBindings(store))

	recorder := serveBinding(t, handler, http.MethodGet, "/bindings", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	// The default page, and the store's one-based index.
	require.Equal(t, [2]int{1, 100}, store.listedAt[0])

	recorder = serveBinding(t, handler, http.MethodGet, "/bindings?limit=25&offset=50", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, [2]int{51, 25}, store.listedAt[1])

	// The page a caller can ask for is bounded, and the response says which
	// page it answered so a client can tell a full listing from a page of one.
	recorder = serveBinding(t, handler, http.MethodGet, "/bindings?limit=100000", "")
	require.Equal(t, [2]int{1, 500}, store.listedAt[2])
	body := bindingBody(t, recorder)
	require.EqualValues(t, 500, body["limit"])
	require.EqualValues(t, 0, body["offset"])
	require.Contains(t, body, "total")
}

/* ── the role control reads the project's own roles ────────────────────── */

// `/admin/roles/{mode}/{projectID}` answers a hardcoded admin/editor/viewer for
// a project that carries no role rows. A picker fed by that offers a role the
// project does not have, and the save is then refused by a value the control
// itself supplied — so this surface answers what the project HAS, empty
// included.
func TestTheProjectRolesRouteAnswersWhatTheProjectHas(t *testing.T) {
	store := &stubBindingStore{roles: []string{"admin", "editor", "viewer", "system"}}
	handler := NewHandler(nil, WithSCIMGroupBindings(store))

	recorder := serveBinding(t, handler, http.MethodGet, "/bindings/project_roles/12", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []any{"admin", "editor", "viewer", "system"}, bindingBody(t, recorder)["roles"])

	empty := NewHandler(nil, WithSCIMGroupBindings(&stubBindingStore{}))
	recorder = serveBinding(t, empty, http.MethodGet, "/bindings/project_roles/12", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Empty(t, bindingBody(t, recorder)["roles"],
		"a project with no roles answers an empty list, never a default set")
}

func TestTheProjectRolesRouteRefusesAnUnusableProjectId(t *testing.T) {
	handler := NewHandler(nil, WithSCIMGroupBindings(&stubBindingStore{}))
	for _, target := range []string{"/bindings/project_roles/0", "/bindings/project_roles/-1"} {
		require.Equal(t, http.StatusNotFound,
			serveBinding(t, handler, http.MethodGet, target, "").Code, target)
	}

	unknown := NewHandler(nil, WithSCIMGroupBindings(
		&stubBindingStore{err: scimdirectory.UnknownProjectError{ProjectID: 42}}))
	require.Equal(t, http.StatusNotFound,
		serveBinding(t, unknown, http.MethodGet, "/bindings/project_roles/42", "").Code)
}
