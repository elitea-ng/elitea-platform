package dbactions

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

type fakeRepo struct {
	created   []dbaction.Rule
	updated   []dbaction.Rule
	actor     int64
	expected  int64
	target    repos.DBActionConfigurationTarget
	lookupErr error
	err       error
	names     []repos.DBActionRuleName
}

func (f *fakeRepo) List(context.Context, int64) ([]repos.DBActionRuleRecord, error) {
	return []repos.DBActionRuleRecord{}, f.err
}
func (f *fakeRepo) Get(_ context.Context, projectID, ruleID int64) (repos.DBActionRuleRecord, error) {
	if f.err != nil {
		return repos.DBActionRuleRecord{}, f.err
	}
	return repos.DBActionRuleRecord{Rule: dbaction.Rule{ID: ruleID, ProjectID: projectID}}, nil
}
func (f *fakeRepo) ListNames(context.Context, int64) ([]repos.DBActionRuleName, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.names != nil {
		return f.names, nil
	}
	return []repos.DBActionRuleName{}, nil
}
func (f *fakeRepo) LookupConfigurationTarget(context.Context, int64, int64, int32, string) (repos.DBActionConfigurationTarget, error) {
	if f.lookupErr != nil {
		return repos.DBActionConfigurationTarget{}, f.lookupErr
	}
	if f.target.Host != "" {
		return f.target, nil
	}
	return repos.DBActionConfigurationTarget{Host: "warehouse.example.com", Port: 5432}, nil
}
func (f *fakeRepo) Create(_ context.Context, projectID, actorID int64, rule dbaction.Rule) (repos.DBActionRuleRecord, error) {
	if f.err != nil {
		return repos.DBActionRuleRecord{}, f.err
	}
	f.created, f.actor = append(f.created, rule), actorID
	rule.ID, rule.ProjectID, rule.Revision = 41, projectID, 1
	return repos.DBActionRuleRecord{Rule: rule}, nil
}
func (f *fakeRepo) Update(_ context.Context, projectID, ruleID, actorID int64, rule dbaction.Rule, expected int64) (repos.DBActionRuleRecord, error) {
	if f.err != nil {
		return repos.DBActionRuleRecord{}, f.err
	}
	f.updated, f.actor, f.expected = append(f.updated, rule), actorID, expected
	rule.ID, rule.ProjectID, rule.Revision = ruleID, projectID, expected+1
	return repos.DBActionRuleRecord{Rule: rule}, nil
}

type grantAll struct{}

func (grantAll) ResolvePermissions(_ context.Context, _ auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 5, Permissions: []string{PermissionManage, PermissionList, permissionConfigurationDetails}}, nil
}

type grantManageOnly struct{}

func (grantManageOnly) ResolvePermissions(_ context.Context, _ auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 5, Permissions: []string{PermissionManage}}, nil
}

type fakeResolver map[string][]net.IPAddr

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func testGuard(t *testing.T, allowlist string) *egress.Guard {
	t.Helper()
	guard, err := egress.NewDBActionGuard(allowlist, egress.WithResolver(fakeResolver{
		"warehouse.example.com": {{IP: net.ParseIP("93.184.216.34")}},
		"internal.db.local":     {{IP: net.ParseIP("10.0.0.5")}},
		"metadata.db.local":     {{IP: net.ParseIP("169.254.169.254")}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func validBody() map[string]any {
	return map[string]any{
		"name":             "warehouse.read",
		"configuration_id": 7,
		"dialect":          "postgres",
		"database_name":    "analytics",
		"mode":             "read",
		"enabled":          true,
	}
}

func serve(t *testing.T, h *Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw string
	switch typed := body.(type) {
	case nil:
	case string:
		raw = typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(encoded)
	}
	router := chi.NewRouter()
	router.Mount("/dbactions/prompt_lib/{projectID}", h.Routes())
	request := httptest.NewRequest(method, path, strings.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "5", UserID: "5"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const rulesPath = "/dbactions/prompt_lib/3/rules"

func TestCreateValidationAndEgressBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowlist string
		target    repos.DBActionConfigurationTarget
		mutate    func(map[string]any)
		want      int
		field     string
	}{
		{name: "valid public read rule", mutate: func(map[string]any) {}, want: http.StatusCreated},
		{name: "write rule defaults marker_table", mutate: func(b map[string]any) { b["mode"] = "write" }, want: http.StatusCreated},
		{name: "write rule custom schema.table marker", mutate: func(b map[string]any) { b["mode"] = "write"; b["marker_table"] = "etl.effects" }, want: http.StatusCreated},
		{name: "write rule explicit empty marker refused", mutate: func(b map[string]any) { b["mode"] = "write"; b["marker_table"] = "" }, want: http.StatusBadRequest, field: "marker_table"},
		{name: "read rule with marker_table refused", mutate: func(b map[string]any) { b["marker_table"] = "elitea_effects" }, want: http.StatusBadRequest, field: "marker_table"},
		{name: "write rule injection marker refused", mutate: func(b map[string]any) { b["mode"] = "write"; b["marker_table"] = "t; DROP TABLE x" }, want: http.StatusBadRequest, field: "marker_table"},
		{name: "unsupported dialect", mutate: func(b map[string]any) { b["dialect"] = "sqlite" }, want: http.StatusBadRequest, field: "dialect"},
		{name: "zero configuration_id refused", mutate: func(b map[string]any) { b["configuration_id"] = 0 }, want: http.StatusBadRequest, field: "configuration_id"},
		{name: "missing configuration_id", mutate: func(b map[string]any) { delete(b, "configuration_id") }, want: http.StatusBadRequest, field: "configuration_id"},
		{name: "missing enabled", mutate: func(b map[string]any) { delete(b, "enabled") }, want: http.StatusBadRequest, field: "enabled"},
		{name: "unknown field host refused", mutate: func(b map[string]any) { b["host"] = "db.example.com" }, want: http.StatusBadRequest},
		{name: "unknown field password refused", mutate: func(b map[string]any) { b["password"] = "secret" }, want: http.StatusBadRequest},
		{name: "private host empty allowlist", target: repos.DBActionConfigurationTarget{Host: "internal.db.local", Port: 5432}, mutate: func(map[string]any) {}, want: http.StatusBadRequest, field: "destination"},
		{name: "metadata host never allowed", allowlist: "10.0.0.0/8", target: repos.DBActionConfigurationTarget{Host: "metadata.db.local", Port: 5432}, mutate: func(map[string]any) {}, want: http.StatusBadRequest, field: "destination"},
		{name: "private host allowlisted port match", allowlist: "10.0.0.5:5432", target: repos.DBActionConfigurationTarget{Host: "internal.db.local", Port: 5432}, mutate: func(map[string]any) {}, want: http.StatusCreated},
		{name: "private host allowlisted wrong port", allowlist: "10.0.0.5:3306", target: repos.DBActionConfigurationTarget{Host: "internal.db.local", Port: 5432}, mutate: func(map[string]any) {}, want: http.StatusBadRequest, field: "destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{target: tc.target}
			h := NewHandler(repo, testGuard(t, tc.allowlist), grantAll{})
			body := validBody()
			tc.mutate(body)
			response := serve(t, h, http.MethodPost, rulesPath, body)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.want != http.StatusCreated {
				if len(repo.created) != 0 {
					t.Fatal("a refused rule reached the repository")
				}
				message := response.Body.String()
				if tc.field != "" && !strings.Contains(message, tc.field) {
					t.Fatalf("refusal does not name %s: %s", tc.field, message)
				}
				for _, leaked := range []string{"internal.db.local", "metadata.db.local", "10.0.0.5", "169.254.169.254", "DROP"} {
					if strings.Contains(message, leaked) {
						t.Fatalf("refusal leaked sensitive value %q: %s", leaked, message)
					}
				}
				return
			}
			if len(repo.created) != 1 || repo.actor != 5 || repo.created[0].SchemaVersion != dbaction.RuleSchema {
				t.Fatalf("repository received %+v (actor %d)", repo.created, repo.actor)
			}
			if body["mode"] == "write" && body["marker_table"] == nil && repo.created[0].MarkerTable != dbaction.DefaultMarkerTable {
				t.Fatalf("default marker_table not applied: %+v", repo.created[0])
			}
		})
	}
}

func TestRuleNamesExposesOnlyRedactedFields(t *testing.T) {
	repo := &fakeRepo{
		names: []repos.DBActionRuleName{
			{Name: "warehouse.read", Dialect: "postgres", Mode: "read", Enabled: true},
			{Name: "warehouse.load", Dialect: "mysql", Mode: "write", Enabled: false},
		},
	}
	h := NewHandler(repo, testGuard(t, ""), grantAll{})
	response := serve(t, h, http.MethodGet, "/dbactions/prompt_lib/3/rule-names", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET rule-names = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Items) != 2 {
		t.Fatalf("decoded = %+v, err = %v", payload, err)
	}
	for _, item := range payload.Items {
		if len(item) != 4 {
			t.Fatalf("rule-names item has unexpected fields: %+v", item)
		}
		for _, forbidden := range []string{"configuration_id", "database_name", "marker_table", "host", "password"} {
			if _, exists := item[forbidden]; exists {
				t.Fatalf("rule-names leaked forbidden field %q", forbidden)
			}
		}
	}
}

func TestBodyCapAndTrailingData(t *testing.T) {
	h := NewHandler(&fakeRepo{}, testGuard(t, ""), grantAll{})
	body := validBody()
	body["database_name"] = strings.Repeat("a", MaxRequestBodyBytes)
	if response := serve(t, h, http.MethodPost, rulesPath, body); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), fmt.Sprint(MaxRequestBodyBytes)) {
		t.Fatalf("oversized body = %d %s", response.Code, response.Body.String())
	}
	encoded, _ := json.Marshal(validBody())
	if response := serve(t, h, http.MethodPost, rulesPath, string(encoded)+`{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d", response.Code)
	}
	padded := string(encoded[:len(encoded)-1]) + strings.Repeat(" ", MaxRequestBodyBytes-len(encoded)) + "}"
	if response := serve(t, h, http.MethodPost, rulesPath, padded); response.Code != http.StatusCreated {
		t.Fatalf("exact cap body = %d %s", response.Code, response.Body.String())
	}
	if response := serve(t, h, http.MethodPost, rulesPath, padded+" "); response.Code != http.StatusBadRequest {
		t.Fatalf("cap+1 body = %d", response.Code)
	}
}

func TestNilGuardNilRepoAndUpdateRevision(t *testing.T) {
	repo := &fakeRepo{}
	if response := serve(t, NewHandler(repo, nil, grantAll{}), http.MethodPost, rulesPath, validBody()); response.Code != http.StatusBadRequest || len(repo.created) != 0 {
		t.Fatalf("nil guard create = %d", response.Code)
	}
	hNilRepo := NewHandler(nil, testGuard(t, ""), grantAll{})
	for _, path := range []string{rulesPath, rulesPath + "/4", "/dbactions/prompt_lib/3/rule-names"} {
		if response := serve(t, hNilRepo, http.MethodGet, path, nil); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s without repo = %d", path, response.Code)
		}
	}

	h := NewHandler(repo, testGuard(t, ""), grantAll{})
	if response := serve(t, h, http.MethodPut, rulesPath+"/4", validBody()); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "revision") {
		t.Fatalf("update without revision = %d %s", response.Code, response.Body.String())
	}
	body := validBody()
	body["revision"] = 3
	response := serve(t, h, http.MethodPut, rulesPath+"/4", body)
	if response.Code != http.StatusOK || repo.expected != 3 || repo.actor != 5 {
		t.Fatalf("update = %d (expected %d, actor %d)", response.Code, repo.expected, repo.actor)
	}
}

func TestRepositoryErrorsMapToStatuses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{repos.ErrDBActionRuleNotFound, http.StatusNotFound},
		{repos.ErrDBActionRuleNameTaken, http.StatusConflict},
		{repos.ErrDBActionRuleStale, http.StatusConflict},
		{repos.ErrDBActionRuleLimit, http.StatusConflict},
		{repos.ErrDBActionRuleConfigurationInvalid, http.StatusBadRequest},
		{fmt.Errorf("wrapped: %w", repos.ErrDBActionRuleStoredInvalid), http.StatusInternalServerError},
		{fmt.Errorf("boom"), http.StatusInternalServerError},
	} {
		h := NewHandler(&fakeRepo{err: tc.err}, testGuard(t, ""), grantAll{})
		body := validBody()
		body["revision"] = 1
		for _, req := range []struct{ method, path string }{
			{http.MethodPost, rulesPath},
			{http.MethodPut, rulesPath + "/4"},
			{http.MethodGet, rulesPath + "/4"},
		} {
			if req.method == http.MethodGet && tc.status == http.StatusConflict {
				continue
			}
			response := serve(t, h, req.method, req.path, body)
			if response.Code != tc.status {
				t.Fatalf("%s %s with %v = %d, want %d", req.method, req.path, tc.err, response.Code, tc.status)
			}
		}
	}
}

func TestMalformedIDsAuditAndConfigurationPermission(t *testing.T) {
	h := NewHandler(&fakeRepo{}, testGuard(t, ""), grantAll{})
	for _, path := range []string{rulesPath + "/abc", rulesPath + "/0", rulesPath + "/-1", rulesPath + "/01"} {
		if response := serve(t, h, http.MethodGet, path, nil); response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, response.Code)
		}
	}

	router := chi.NewRouter()
	router.Mount("/dbactions/prompt_lib/{projectID}", h.Routes())
	encoded, _ := json.Marshal(validBody())
	request := httptest.NewRequest(http.MethodPost, rulesPath, strings.NewReader(string(encoded)))
	ctx, slot := audit.ContextWithAnnotationSlot(auth.ContextWithUser(request.Context(), auth.User{ID: "5", UserID: "5"}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request.WithContext(ctx))
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d", response.Code)
	}
	annotation, ok := slot.Read()
	if !ok || annotation.EntityType != AuditEntityType || annotation.EntityID == nil || *annotation.EntityID != 41 ||
		annotation.ProjectID == nil || *annotation.ProjectID != 3 {
		t.Fatalf("annotation = %+v (%v)", annotation, ok)
	}

	// Manage-only caller without configurations.configuration.details cannot bind configuration_id.
	repo := &fakeRepo{}
	hManageOnly := NewHandler(repo, testGuard(t, ""), grantManageOnly{})
	if resp := serve(t, hManageOnly, http.MethodPost, rulesPath, validBody()); resp.Code != http.StatusForbidden || len(repo.created) != 0 {
		t.Fatalf("manage-only admin bound configuration_id: %d %s", resp.Code, resp.Body.String())
	}
}
