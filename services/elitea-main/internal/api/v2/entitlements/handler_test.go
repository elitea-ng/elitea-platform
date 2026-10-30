package entitlements

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type fakeStore struct {
	grants     []domain.Grant
	audits     []repos.EntitlementExportAudit
	auditFails bool
	filter     repos.EntitlementFilter
	limit      int
	offset     int
}

func (f *fakeStore) Stream(_ context.Context, filter repos.EntitlementFilter,
	before func(int) error, emit func(domain.Grant) error) error {
	f.filter = filter
	if err := before(len(f.grants)); err != nil {
		return err
	}
	for _, grant := range f.grants {
		if err := emit(grant); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStore) Page(_ context.Context, filter repos.EntitlementFilter, limit, offset int) ([]domain.Grant, int, error) {
	f.filter, f.limit, f.offset = filter, limit, offset
	return f.grants, 42, nil
}

func (f *fakeStore) RecordExport(_ context.Context, record repos.EntitlementExportAudit) error {
	if f.auditFails {
		return errors.New("down")
	}
	f.audits = append(f.audits, record)
	return nil
}

func fixtureStore() *fakeStore {
	return &fakeStore{grants: []domain.Grant{
		{UserID: 7, UserName: "a@x.io", DisplayName: "Smith, \"J\"\nJr", UserActive: true,
			ScopeType: "project", ScopeID: "3", ScopeName: "Alpha", ProjectID: 3, ScopeActive: true,
			Role: "admin", RoleID: 9, Capability: "publish_agents",
			Permissions: []string{"models.applications.publish.post"}, GrantedVia: "role"},
		{UserID: 7, UserName: "a@x.io", DisplayName: "Smith", UserActive: true,
			ScopeType: "platform", ScopeID: "administration", ScopeName: "Platform (administration)", ScopeActive: true,
			Role: "viewer", RoleID: 7, GrantedVia: "role"},
	}}
}

func serve(store Store, target string, user *auth.User) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Get(AdminPath, NewHandler(store).Export)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

var operator = &auth.User{ID: "1", UserID: "1", Email: "ops@x.io"}

func TestCSVExportStreamsQuotedRowsAndIsAudited(t *testing.T) {
	store := fixtureStore()
	recorder := serve(store, "/entitlements?format=csv&project_id=3", operator)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "text/csv; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.Equal(t, `attachment; filename="entitlements.csv"`, recorder.Header().Get("Content-Disposition"))
	require.Equal(t, "2", recorder.Header().Get(RowsHeader))
	require.Equal(t, int64(3), store.filter.ProjectID)

	body := recorder.Body.String()
	require.True(t, strings.HasPrefix(body, strings.Join(domain.CSVHeader, ",")+"\r\n"))
	require.Contains(t, body, "\"Smith, \"\"J\"\"\r\nJr\"")
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.Equal(t, "Smith, \"J\"\nJr", records[1][2]) // a reader folds CRLF in a field
	require.Equal(t, []string{"7", "a@x.io", "Smith", "true", "platform", "administration",
		"Platform (administration)", "", "true", "viewer", "", "", "role"}, records[2])

	require.Equal(t, []repos.EntitlementExportAudit{{UserID: 1, Actor: "ops@x.io", Format: "csv", Rows: 2, Route: AdminPath}},
		store.audits)
}

func TestJSONExportPagesAndIsAudited(t *testing.T) {
	store := fixtureStore()
	recorder := serve(store, "/entitlements?limit=2&offset=4&user_id=7", operator)
	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Total int     `json:"total"`
		Rows  []Grant `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, 42, body.Total)
	require.Len(t, body.Rows, 2)
	require.Nil(t, body.Rows[1].Capability)
	require.Nil(t, body.Rows[1].ProjectID)
	require.Equal(t, []string{}, body.Rows[1].Permissions)
	require.Equal(t, 2, store.limit)
	require.Equal(t, 4, store.offset)
	require.Equal(t, int64(7), store.filter.UserID)
	require.Equal(t, "json", store.audits[0].Format)
	require.Equal(t, 2, store.audits[0].Rows)
}

func TestExportRefusals(t *testing.T) {
	store := fixtureStore()
	require.Equal(t, http.StatusBadRequest, serve(store, "/entitlements?format=xml", operator).Code)
	require.Equal(t, http.StatusBadRequest, serve(store, "/entitlements?user_id=0", operator).Code)
	require.Equal(t, http.StatusBadRequest, serve(store, "/entitlements?limit=501", operator).Code)
	require.Equal(t, http.StatusUnauthorized, serve(store, "/entitlements", nil).Code)
	require.Equal(t, http.StatusServiceUnavailable, serve(nil, "/entitlements", operator).Code)

	// An export the audit trail cannot record leaves no row.
	store.auditFails = true
	for _, target := range []string{"/entitlements?format=csv", "/entitlements"} {
		recorder := serve(store, target, operator)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, target)
		require.NotContains(t, recorder.Body.String(), "a@x.io")
		require.Empty(t, recorder.Header().Get(RowsHeader))
	}
}
