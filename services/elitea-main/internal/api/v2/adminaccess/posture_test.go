package adminaccess_test

// The access posture route (ADR-0059 R2.14a, issue 2528) behind its gate,
// with fake checks. The checks themselves, on PostgreSQL, are
// posture_postgres_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// postureResponse is the wire shape the tests read.
type postureResponse struct {
	Checks []struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		State    string `json:"state"`
		TextKey  string `json:"text_key"`
		Text     string `json:"text"`
		Facts    struct {
			Count  int      `json:"count"`
			Roles  []string `json:"roles"`
			People []struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"people"`
		} `json:"facts"`
	} `json:"checks"`
	Failures  int       `json:"failures"`
	Warnings  int       `json:"warnings"`
	Notices   int       `json:"notices"`
	CheckedAt time.Time `json:"checked_at"`
}

func (p postureResponse) check(t *testing.T, id string) (severity, state, text string, count int, roles []string) {
	t.Helper()
	for _, c := range p.Checks {
		if c.ID == id {
			return c.Severity, c.State, c.Text, c.Facts.Count, c.Facts.Roles
		}
	}
	t.Fatalf("the list has no check %s", id)
	return "", "", "", 0, nil
}

// servePosture drives GET /access/posture behind the gate the production
// router puts in front of it.
func servePosture(t *testing.T, handler *console.PostureHandler, resolver auth.PermissionResolver, user auth.User) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	handler.Register(r, apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration,
		registry.AccessRolesView, registry.AdminAuthUsers, registry.ModesUsers))
	req := httptest.NewRequest(http.MethodGet, console.PosturePath, nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodePosture(t *testing.T, rec *httptest.ResponseRecorder) postureResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("posture: %d %s", rec.Code, rec.Body)
	}
	var body postureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// postureDirectory: user 1 is a security auditor (access.roles.view), user 2
// an LLM manager (no Access permission).
func postureDirectory() *directory {
	d := newDirectory()
	d.users[1] = []string{registry.RoleSecurityAuditor}
	d.users[2] = []string{registry.RoleLLMManager}
	return d
}

func fixedCheck(id string, severity registry.PostureSeverity, pass bool) registry.PostureCheck {
	return registry.PostureCheck{ID: id, Severity: severity,
		Run: func(context.Context, registry.PostureSources) (registry.PostureFinding, error) {
			return registry.PostureFinding{Pass: pass, Text: id, Facts: registry.PostureFacts{Count: 3}}, nil
		}}
}

// A holder of access.roles.view reads the list; only a fail that does not
// pass is counted as a failure, and an info finding (the super_admin count)
// is a notice. A caller without the permission gets 403 and runs nothing.
func TestThePostureRouteCountsOnlyFailsAsFailures(t *testing.T) {
	t.Parallel()
	ran := 0
	counting := func(check registry.PostureCheck) registry.PostureCheck {
		run := check.Run
		check.Run = func(ctx context.Context, s registry.PostureSources) (registry.PostureFinding, error) {
			ran++
			return run(ctx, s)
		}
		return check
	}
	handler := console.NewPostureHandler(registry.PostureSources{DB: unusedQuerier{}}).WithChecks([]registry.PostureCheck{
		counting(fixedCheck("broken_role", registry.PostureFail, false)),
		counting(fixedCheck("fine_role", registry.PostureFail, true)),
		counting(fixedCheck("tokens", registry.PostureWarn, false)),
		counting(fixedCheck(registry.PostureSuperAdminHolders, registry.PostureInfo, false)),
	})
	d := postureDirectory()
	body := decodePosture(t, servePosture(t, handler, d, auth.User{ID: "1", UserID: "1"}))
	if body.Failures != 1 || body.Warnings != 1 || body.Notices != 1 || len(body.Checks) != 4 {
		t.Fatalf("failures %d, warnings %d, notices %d, %d checks; want 1, 1, 1, 4",
			body.Failures, body.Warnings, body.Notices, len(body.Checks))
	}
	severity, state, _, count, roles := body.check(t, registry.PostureSuperAdminHolders)
	if severity != "info" || state != registry.PostureStateAttention || count != 3 || roles == nil {
		t.Fatalf("the super_admin count: %s %s %d %v", severity, state, count, roles)
	}
	if _, state, _, _, _ := body.check(t, "fine_role"); state != registry.PostureStatePass {
		t.Fatalf("a passing check: %s", state)
	}
	for _, c := range body.Checks {
		if c.TextKey != "admin.access.posture."+c.ID {
			t.Errorf("%s: text key %q", c.ID, c.TextKey)
		}
	}

	ran = 0
	if rec := servePosture(t, handler, d, auth.User{ID: "2", UserID: "2"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a caller without access.roles.view: %d, want 403", rec.Code)
	}
	if ran != 0 {
		t.Fatal("a refused caller ran the checks")
	}
}

func TestThePostureRouteFailsClosed(t *testing.T) {
	t.Parallel()
	d := postureDirectory()
	holder := auth.User{ID: "1", UserID: "1"}
	if rec := servePosture(t, console.NewPostureHandler(registry.PostureSources{}), d, holder); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no database: %d, want 503", rec.Code)
	}
	broken := console.NewPostureHandler(registry.PostureSources{DB: unusedQuerier{}}).WithChecks([]registry.PostureCheck{{
		ID: "broken", Severity: registry.PostureInfo,
		Run: func(context.Context, registry.PostureSources) (registry.PostureFinding, error) {
			return registry.PostureFinding{}, errors.New("unreadable")
		},
	}})
	if rec := servePosture(t, broken, d, holder); rec.Code != http.StatusInternalServerError {
		t.Fatalf("an unreadable check: %d, want 500", rec.Code)
	}
}

// unusedQuerier stands for the database of a handler whose checks are fakes.
type unusedQuerier struct{}

func (unusedQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unusedQuerier: no database")
}

func (unusedQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
