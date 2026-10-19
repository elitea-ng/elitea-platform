package httpactions

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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// fakeRepo records what reached the repository.
type fakeRepo struct {
	created  []httpaction.Rule
	updated  []httpaction.Rule
	actor    int64
	expected int64
	err      error
}

func (f *fakeRepo) List(context.Context, int64) ([]repos.HTTPActionRuleRecord, error) {
	return []repos.HTTPActionRuleRecord{}, f.err
}
func (f *fakeRepo) Get(_ context.Context, projectID, ruleID int64) (repos.HTTPActionRuleRecord, error) {
	if f.err != nil {
		return repos.HTTPActionRuleRecord{}, f.err
	}
	return repos.HTTPActionRuleRecord{Rule: httpaction.Rule{ID: ruleID, ProjectID: projectID}}, nil
}
func (f *fakeRepo) ListNames(context.Context, int64) ([]repos.HTTPActionRuleName, error) {
	return []repos.HTTPActionRuleName{}, f.err
}
func (f *fakeRepo) Create(_ context.Context, projectID, actorID int64, rule httpaction.Rule) (repos.HTTPActionRuleRecord, error) {
	if f.err != nil {
		return repos.HTTPActionRuleRecord{}, f.err
	}
	f.created, f.actor = append(f.created, rule), actorID
	rule.ID, rule.ProjectID, rule.Revision = 41, projectID, 1
	return repos.HTTPActionRuleRecord{Rule: rule}, nil
}
func (f *fakeRepo) Update(_ context.Context, projectID, ruleID, actorID int64, rule httpaction.Rule, expected int64) (repos.HTTPActionRuleRecord, error) {
	if f.err != nil {
		return repos.HTTPActionRuleRecord{}, f.err
	}
	f.updated, f.actor, f.expected = append(f.updated, rule), actorID, expected
	rule.ID, rule.ProjectID, rule.Revision = ruleID, projectID, expected+1
	return repos.HTTPActionRuleRecord{Rule: rule}, nil
}

// grantAll resolves every permission for every caller; the gate itself is
// measured by the router-level matrix (internal/api).
type grantAll struct{}

func (grantAll) ResolvePermissions(_ context.Context, user auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 5, Permissions: []string{PermissionManage, PermissionList}}, nil
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
	guard, err := egress.NewHTTPActionGuard(allowlist, egress.WithResolver(fakeResolver{
		"api.example.com":      {{IP: net.ParseIP("93.184.216.34")}},
		"internal.example.com": {{IP: net.ParseIP("10.0.0.5")}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func validBody() map[string]any {
	return map[string]any{
		"name": "orders.lookup", "origin": "https://api.example.com", "path_template": "/v1/orders/{id}",
		"method": "GET", "query_names": []string{"page"}, "configuration_id": 0, "toolkit": "openapi",
		"tool": "get_order", "enabled": true,
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
	router.Mount("/httpactions/prompt_lib/{projectID}", h.Routes())
	request := httptest.NewRequest(method, path, strings.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "5", UserID: "5"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const rulesPath = "/httpactions/prompt_lib/3/rules"

func TestCreateValidationBoundaries(t *testing.T) {
	queryNames := func(n int) []string {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("q%d", i)
		}
		return names
	}
	for _, test := range []struct {
		name      string
		allowlist string
		mutate    func(map[string]any)
		want      int
		field     string
	}{
		{name: "valid public origin", mutate: func(map[string]any) {}, want: http.StatusCreated},
		{name: "32 query names", mutate: func(b map[string]any) { b["query_names"] = queryNames(httpaction.MaxQueryNames) }, want: http.StatusCreated},
		{name: "33 query names", mutate: func(b map[string]any) { b["query_names"] = queryNames(httpaction.MaxQueryNames + 1) }, want: http.StatusBadRequest, field: "query_names"},
		{name: "http origin", mutate: func(b map[string]any) { b["origin"] = "http://api.example.com" }, want: http.StatusBadRequest, field: "origin"},
		{name: "origin with path", mutate: func(b map[string]any) { b["origin"] = "https://api.example.com/v1" }, want: http.StatusBadRequest, field: "origin"},
		{name: "dot segment", mutate: func(b map[string]any) { b["path_template"] = "/v1/../admin" }, want: http.StatusBadRequest, field: "path_template"},
		{name: "private literal, empty allowlist", mutate: func(b map[string]any) { b["origin"] = "https://10.0.0.5" }, want: http.StatusBadRequest, field: "origin"},
		{name: "private host, empty allowlist", mutate: func(b map[string]any) { b["origin"] = "https://internal.example.com" }, want: http.StatusBadRequest, field: "origin"},
		{name: "unresolvable host", mutate: func(b map[string]any) { b["origin"] = "https://nowhere.example.com" }, want: http.StatusBadRequest, field: "origin"},
		{name: "private literal, allowlisted", allowlist: "10.0.0.5", mutate: func(b map[string]any) { b["origin"] = "https://10.0.0.5" }, want: http.StatusCreated},
		{name: "private host, allowlisted", allowlist: "10.0.0.0/8", mutate: func(b map[string]any) { b["origin"] = "https://internal.example.com" }, want: http.StatusCreated},
		{name: "missing enabled", mutate: func(b map[string]any) { delete(b, "enabled") }, want: http.StatusBadRequest, field: "enabled"},
		{name: "missing configuration_id", mutate: func(b map[string]any) { delete(b, "configuration_id") }, want: http.StatusBadRequest, field: "configuration_id"},
		{name: "missing query_names", mutate: func(b map[string]any) { delete(b, "query_names") }, want: http.StatusBadRequest, field: "query_names"},
		{name: "unknown field", mutate: func(b map[string]any) { b["schema_version"] = httpaction.RuleSchema }, want: http.StatusBadRequest},
		{name: "unknown field id", mutate: func(b map[string]any) { b["id"] = 9 }, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeRepo{}
			h := NewHandler(repo, testGuard(t, test.allowlist), grantAll{})
			body := validBody()
			test.mutate(body)
			response := serve(t, h, http.MethodPost, rulesPath, body)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if test.want != http.StatusCreated {
				if len(repo.created) != 0 {
					t.Fatal("a refused rule reached the repository")
				}
				message := response.Body.String()
				if test.field != "" && !strings.Contains(message, test.field) {
					t.Fatalf("refusal does not name %s: %s", test.field, message)
				}
				for _, echoed := range []string{"10.0.0.5", "internal.example.com", "nowhere.example.com", "api.example.com", "/v1/../admin"} {
					if strings.Contains(message, echoed) {
						t.Fatalf("refusal echoes the submitted value %q: %s", echoed, message)
					}
				}
				return
			}
			if len(repo.created) != 1 || repo.actor != 5 || repo.created[0].Replay != httpaction.ReplayNever ||
				repo.created[0].SchemaVersion != httpaction.RuleSchema {
				t.Fatalf("repository received %+v (actor %d)", repo.created, repo.actor)
			}
		})
	}
}

func TestBodyCapAndTrailingData(t *testing.T) {
	h := NewHandler(&fakeRepo{}, testGuard(t, ""), grantAll{})
	body := validBody()
	body["tool"] = strings.Repeat("a", MaxRequestBodyBytes)
	if response := serve(t, h, http.MethodPost, rulesPath, body); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), fmt.Sprint(MaxRequestBodyBytes)) {
		t.Fatalf("oversized body = %d %s", response.Code, response.Body.String())
	}
	encoded, _ := json.Marshal(validBody())
	if response := serve(t, h, http.MethodPost, rulesPath, string(encoded)+`{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d", response.Code)
	}
	if response := serve(t, h, http.MethodPost, rulesPath, `not json`); response.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", response.Code)
	}
}

// A body just under the cap is decoded; the cap is not an off-by-one refusal.
func TestBodyAtTheCapIsAccepted(t *testing.T) {
	h := NewHandler(&fakeRepo{}, testGuard(t, ""), grantAll{})
	encoded, _ := json.Marshal(validBody())
	padded := string(encoded[:len(encoded)-1]) + strings.Repeat(" ", MaxRequestBodyBytes-len(encoded)) + "}"
	if len(padded) != MaxRequestBodyBytes {
		t.Fatalf("fixture is %d bytes", len(padded))
	}
	if response := serve(t, h, http.MethodPost, rulesPath, padded); response.Code != http.StatusCreated {
		t.Fatalf("body of exactly %d bytes = %d %s", MaxRequestBodyBytes, response.Code, response.Body.String())
	}
	if response := serve(t, h, http.MethodPost, rulesPath, padded+" "); response.Code != http.StatusBadRequest {
		t.Fatalf("body of %d bytes = %d", MaxRequestBodyBytes+1, response.Code)
	}
}

func TestNilGuardRefusesWrites(t *testing.T) {
	repo := &fakeRepo{}
	h := NewHandler(repo, nil, grantAll{})
	if response := serve(t, h, http.MethodPost, rulesPath, validBody()); response.Code != http.StatusBadRequest || len(repo.created) != 0 {
		t.Fatalf("nil guard create = %d (%d stored)", response.Code, len(repo.created))
	}
	body := validBody()
	body["revision"] = 1
	if response := serve(t, h, http.MethodPut, rulesPath+"/4", body); response.Code != http.StatusBadRequest || len(repo.updated) != 0 {
		t.Fatalf("nil guard update = %d", response.Code)
	}
}

func TestNilRepositoryAnswersUnavailable(t *testing.T) {
	h := NewHandler(nil, testGuard(t, ""), grantAll{})
	for _, path := range []string{rulesPath, rulesPath + "/4", "/httpactions/prompt_lib/3/rule-names"} {
		if response := serve(t, h, http.MethodGet, path, nil); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s without a repository = %d", path, response.Code)
		}
	}
}

func TestUpdateNeedsRevisionAndPassesIt(t *testing.T) {
	repo := &fakeRepo{}
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
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["revision"] != float64(4) || decoded["id"] != float64(4) {
		t.Fatalf("update response = %v", decoded)
	}
}

func TestRepositoryErrorsMapToStatuses(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{repos.ErrHTTPActionRuleNotFound, http.StatusNotFound},
		{repos.ErrHTTPActionRuleNameTaken, http.StatusConflict},
		{repos.ErrHTTPActionRuleStale, http.StatusConflict},
		{repos.ErrHTTPActionRuleLimit, http.StatusConflict},
		{fmt.Errorf("wrapped: %w", repos.ErrHTTPActionRuleStoredInvalid), http.StatusInternalServerError},
		{fmt.Errorf("boom"), http.StatusInternalServerError},
	} {
		h := NewHandler(&fakeRepo{err: test.err}, testGuard(t, ""), grantAll{})
		body := validBody()
		body["revision"] = 1
		for _, request := range []struct{ method, path string }{
			{http.MethodPost, rulesPath}, {http.MethodPut, rulesPath + "/4"}, {http.MethodGet, rulesPath + "/4"},
		} {
			if request.method == http.MethodGet && test.status == http.StatusConflict {
				continue
			}
			response := serve(t, h, request.method, request.path, body)
			if response.Code != test.status {
				t.Fatalf("%s %s with %v = %d, want %d", request.method, request.path, test.err, response.Code, test.status)
			}
		}
	}
}

func TestMalformedIDsAreNotFound(t *testing.T) {
	h := NewHandler(&fakeRepo{}, testGuard(t, ""), grantAll{})
	for _, path := range []string{rulesPath + "/abc", rulesPath + "/0", rulesPath + "/-1", rulesPath + "/01"} {
		if response := serve(t, h, http.MethodGet, path, nil); response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, response.Code)
		}
	}
}

func TestWritesAnnotateTheAuditRow(t *testing.T) {
	h := NewHandler(&fakeRepo{}, testGuard(t, ""), grantAll{})
	router := chi.NewRouter()
	router.Mount("/httpactions/prompt_lib/{projectID}", h.Routes())
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
}
