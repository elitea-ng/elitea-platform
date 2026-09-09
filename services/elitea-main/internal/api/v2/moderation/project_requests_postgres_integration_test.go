package moderation_test

// Acceptance for self-service "request a project" (issue #871).
//
// Unlike requests_postgres_integration_test.go's harness (001_initial.sql
// alone), a Project Request's APPROVAL runs the real project-creation
// pipeline — tenant schema, vault, RBAC roles, the lot — so these tests need
// the full ledgered migration corpus, not just the bootstrap. Same template
// technique projectprovisioning's own integration suite uses
// (dbtest.EnsureTemplate, built once in TestMain and copied per test).
//
// What each test pins:
//
//	TestProjectRequestIsScopedToTheRequestersPersonalProject
//	    filing a request resolves (and provisions, since it does not exist
//	    yet) the CALLER's personal project as the row's project_id — the
//	    bookkeeping value project_requests.go's header describes.
//	TestApprovingAProjectRequestProvisionsTheProjectWithTheRequesterAsAdmin
//	    the whole point of the issue: approval does not just flip a status
//	    column, it creates a real, usable project and makes the requester
//	    its admin.
//	TestRejectingAProjectRequestProvisionsNothing
//	    the mirror image — a rejection changes status and nothing else.
//	TestApprovingAProjectRequestTwiceProvisionsOnlyOnce
//	    the row lock in decideProjectRequest: a second decision on an
//	    already-decided row is refused, not re-run.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/moderation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const projectRequestDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
const projectRequestBootstrapSchema = "../../../infra/db/migrations/001_initial.sql"

var projectRequestTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(projectRequestDatabaseURLEnv)
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(projectRequestBootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files:   platformmigrations.Files,
		Seed:    string(bootstrap),
		Tenants: []int64{1},
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build project request template: %v\n", err)
		os.Exit(1)
	}
	projectRequestTemplate = templateName
	os.Exit(m.Run())
}

// newProjectRequestPool mirrors projectprovisioning's newProvisioningPool:
// one template built once, copied per test.
func newProjectRequestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(projectRequestDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", projectRequestDatabaseURLEnv)
	}
	if projectRequestTemplate == "" {
		t.Fatalf("TestMain did not build the project request template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_project_request_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, projectRequestTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", projectRequestDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// newProjectRequestHandler wires a moderation.Handler with the REAL
// provisioning pipeline and the REAL personal-project ensurer — the same
// composition internal/api/router.go builds — so an approval in these tests
// runs the actual project-creation code path, not a stub.
func newProjectRequestHandler(t *testing.T, pool *pgxpool.Pool) *moderation.Handler {
	t.Helper()
	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
	ensurer, err := personalproject.NewEnsurer(pool, provisioner)
	if err != nil {
		t.Fatalf("build personal project ensurer: %v", err)
	}
	return moderation.NewHandler(pool,
		moderation.WithProjectProvisioner(provisioner),
		moderation.WithPersonalProjectEnsurer(ensurer),
	)
}

const (
	projectRequestCreateURL = "/admin/moderation_status/project_request"
	projectRequestMineURL   = "/admin/moderation_status/project_requests/mine"
	projectRequestDecideURL = "/admin/moderation_status/administration"
)

func projectRequestRouter(handler *moderation.Handler, principal auth.User) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Post(projectRequestCreateURL, handler.CreateProjectRequest)
	router.Get(projectRequestMineURL, handler.MyProjectRequests)
	router.Put(projectRequestDecideURL, handler.AdministrationRequestUpdate)
	// `queueURL` (requests_postgres_integration_test.go, same package): the
	// admin queue read, needed here so an approved Project Request's
	// `created_project_id` can be asserted on the LISTED row, not only on
	// the one-shot decide response (see TestAdminQueueSurfacesTheCreatedProjectID).
	router.Get(queueURL, handler.AdministrationRequests)
	return router
}

func doJSON(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	reader := bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// seedUser inserts one auth_core__user row so `personalproject.Ensurer`'s
// eligibility read and the moderation queue's email join both have something
// real to find. Ids are chosen far from p_1's seeded users (1, and whatever
// the corpus itself creates) to avoid colliding with the template's own rows.
func seedUser(t *testing.T, pool *pgxpool.Pool, id int64, email string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO auth_core__user (id, email, name, suspended) VALUES ($1, $2, $3, false)
ON CONFLICT (id) DO NOTHING`, id, email, email); err != nil {
		t.Fatalf("seed user %d: %v", id, err)
	}
}

func projectRow(t *testing.T, pool *pgxpool.Pool, id int64) (name string, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT name FROM centry.project WHERE id = $1`, id).Scan(&name)
	if err != nil {
		return "", false
	}
	return name, true
}

func isProjectAdmin(t *testing.T, pool *pgxpool.Pool, projectID, userID int64) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
  SELECT 1 FROM public.auth_core__project_user_role pur
  JOIN public.auth_core__project_role pr ON pr.id = pur.role_id
  WHERE pur.project_id = $1 AND pur.user_id = $2 AND pr.name = 'admin'
)`, projectID, userID).Scan(&exists)
	if err != nil {
		t.Fatalf("check project admin membership: %v", err)
	}
	return exists
}

func countProjectsNamed(t *testing.T, pool *pgxpool.Pool, name string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.project WHERE name = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count projects named %q: %v", name, err)
	}
	return count
}

/* ── submit ────────────────────────────────────────────────────────────── */

func TestProjectRequestIsScopedToTheRequestersPersonalProject(t *testing.T) {
	pool := newProjectRequestPool(t)
	handler := newProjectRequestHandler(t, pool)
	const userID int64 = 90001
	seedUser(t, pool, userID, "requester-scope@example.com")
	principal := auth.User{ID: fmt.Sprint(userID), UserID: fmt.Sprint(userID), Email: "requester-scope@example.com"}
	router := projectRequestRouter(handler, principal)

	recorder := doJSON(t, router, http.MethodPost, projectRequestCreateURL, map[string]string{
		"name":        "Scope Test Project",
		"description": "Need it for testing.",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID        int64  `json:"id"`
		ProjectID int64  `json:"project_id"`
		IssueType string `json:"issue_type"`
		EntityID  string `json:"entity_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.IssueType != moderation.ProjectRequestIssueType {
		t.Errorf("issue_type = %q, want %q", created.IssueType, moderation.ProjectRequestIssueType)
	}
	if created.EntityID != "Scope Test Project" {
		t.Errorf("entity_id = %q, want the requested name", created.EntityID)
	}
	if created.Status != "pending" {
		t.Errorf("status = %q, want pending", created.Status)
	}
	if created.ProjectID <= 0 {
		t.Fatalf("project_id = %d, want the requester's own personal project", created.ProjectID)
	}
	// The row's project_id really is a project: the ensurer provisioned it
	// (this user had none) rather than the handler inventing a number.
	if _, ok := projectRow(t, pool, created.ProjectID); !ok {
		t.Fatalf("project_id %d names no row in centry.project", created.ProjectID)
	}

	// The requester's own read agrees.
	mine := doJSON(t, router, http.MethodGet, projectRequestMineURL, nil)
	if mine.Code != http.StatusOK {
		t.Fatalf("mine: status = %d, body = %s", mine.Code, mine.Body.String())
	}
	var mineBody struct {
		Total int `json:"total"`
		Rows  []struct {
			ID int64 `json:"id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(mine.Body.Bytes(), &mineBody); err != nil {
		t.Fatalf("decode mine response: %v", err)
	}
	if mineBody.Total != 1 || len(mineBody.Rows) != 1 || mineBody.Rows[0].ID != created.ID {
		t.Fatalf("mine = %+v, want exactly the row just created", mineBody)
	}
}

func TestProjectRequestRefusesAnEmptyNameOrDescription(t *testing.T) {
	pool := newProjectRequestPool(t)
	handler := newProjectRequestHandler(t, pool)
	const userID int64 = 90002
	seedUser(t, pool, userID, "requester-empty@example.com")
	principal := auth.User{ID: fmt.Sprint(userID), UserID: fmt.Sprint(userID)}
	router := projectRequestRouter(handler, principal)

	for _, body := range []map[string]string{
		{"description": "no name"},
		{"name": "no description"},
		{"name": "  ", "description": "blank name"},
	} {
		recorder := doJSON(t, router, http.MethodPost, projectRequestCreateURL, body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %+v: status = %d, want 400", body, recorder.Code)
		}
	}
}

/* ── decide: approve ───────────────────────────────────────────────────── */

func TestApprovingAProjectRequestProvisionsTheProjectWithTheRequesterAsAdmin(t *testing.T) {
	pool := newProjectRequestPool(t)
	handler := newProjectRequestHandler(t, pool)
	const requesterID int64 = 90101
	const operatorID int64 = 90102
	seedUser(t, pool, requesterID, "requester-approve@example.com")
	seedUser(t, pool, operatorID, "operator-approve@example.com")
	requester := auth.User{ID: fmt.Sprint(requesterID), UserID: fmt.Sprint(requesterID)}
	operator := auth.User{ID: fmt.Sprint(operatorID), UserID: fmt.Sprint(operatorID)}

	const projectName = "Approved Marketing Automation"
	created := doJSON(t, projectRequestRouter(handler, requester), http.MethodPost, projectRequestCreateURL,
		map[string]string{"name": projectName, "description": "Campaign work."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdRow struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// The operator's decision, over the SAME PUT the App Requests admin page
	// already calls.
	decision := doJSON(t, projectRequestRouter(handler, operator), http.MethodPut, projectRequestDecideURL,
		map[string]any{"id": createdRow.ID, "status": "approved"})
	if decision.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, body = %s", decision.Code, decision.Body.String())
	}
	var decided struct {
		Status           string `json:"status"`
		CreatedProjectID int64  `json:"created_project_id"`
	}
	if err := json.Unmarshal(decision.Body.Bytes(), &decided); err != nil {
		t.Fatalf("decode decision response: %v", err)
	}
	if decided.Status != "approved" {
		t.Fatalf("status = %q, want approved", decided.Status)
	}
	if decided.CreatedProjectID <= 0 {
		t.Fatalf("created_project_id = %d, want a real project id", decided.CreatedProjectID)
	}

	// The project is REAL: it has the requested name, and it exists as more
	// than a bare row — the same pipeline POST /projects/project/administration
	// uses provisions a tenant schema, a vault and RBAC roles for it too.
	name, ok := projectRow(t, pool, decided.CreatedProjectID)
	if !ok {
		t.Fatalf("project %d does not exist", decided.CreatedProjectID)
	}
	if name != projectName {
		t.Errorf("project name = %q, want %q", name, projectName)
	}
	if !isProjectAdmin(t, pool, decided.CreatedProjectID, requesterID) {
		t.Errorf("requester %d is not an admin of project %d", requesterID, decided.CreatedProjectID)
	}
	// The operator who approved it did NOT thereby become its admin.
	if isProjectAdmin(t, pool, decided.CreatedProjectID, operatorID) {
		t.Errorf("operator %d was made an admin of a project it only approved", operatorID)
	}
}

// TestAdminQueueSurfacesTheCreatedProjectID — #882 CI: `requestColumns`
// (requests.go) never selected `meta`, so `created_project_id` — written
// there by decideProjectRequest at approval time — was populated on the
// ONE-SHOT decide response and nowhere else. The admin App Requests page
// (AppRequestsTable.tsx) reads it off the LISTED row to show "Project #N
// created", so every approved Project Request read back through the queue
// looked exactly like a clerical decision with nothing provisioned.
func TestAdminQueueSurfacesTheCreatedProjectID(t *testing.T) {
	pool := newProjectRequestPool(t)
	handler := newProjectRequestHandler(t, pool)
	const requesterID int64 = 90401
	const operatorID int64 = 90402
	seedUser(t, pool, requesterID, "requester-queue@example.com")
	seedUser(t, pool, operatorID, "operator-queue@example.com")
	requester := auth.User{ID: fmt.Sprint(requesterID), UserID: fmt.Sprint(requesterID)}
	operator := auth.User{ID: fmt.Sprint(operatorID), UserID: fmt.Sprint(operatorID)}

	const projectName = "Queue Surfaced Automation"
	created := doJSON(t, projectRequestRouter(handler, requester), http.MethodPost, projectRequestCreateURL,
		map[string]string{"name": projectName, "description": "Queue read coverage."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdRow struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	operatorRouter := projectRequestRouter(handler, operator)
	decision := doJSON(t, operatorRouter, http.MethodPut, projectRequestDecideURL,
		map[string]any{"id": createdRow.ID, "status": "approved"})
	if decision.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, body = %s", decision.Code, decision.Body.String())
	}
	var decided struct {
		CreatedProjectID int64 `json:"created_project_id"`
	}
	if err := json.Unmarshal(decision.Body.Bytes(), &decided); err != nil {
		t.Fatalf("decode decision response: %v", err)
	}
	if decided.CreatedProjectID <= 0 {
		t.Fatalf("created_project_id = %d, want a real project id", decided.CreatedProjectID)
	}

	// THE ASSERTION #882 CI CAUGHT: the SAME id, read back through the
	// admin queue's LISTING, not the decide response.
	listing := readQueue(t, operatorRouter, "status=approved")
	var found *requestRow
	for i := range listing.Rows {
		if listing.Rows[i].ID == createdRow.ID {
			found = &listing.Rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("request %d not found in the approved queue (rows: %+v)", createdRow.ID, listing.Rows)
	}
	if found.CreatedProjectID == nil {
		t.Fatalf("queue row for request %d carries no created_project_id", createdRow.ID)
	}
	if *found.CreatedProjectID != decided.CreatedProjectID {
		t.Errorf("queue row created_project_id = %d, want %d (the decide response's own answer)",
			*found.CreatedProjectID, decided.CreatedProjectID)
	}
}

/* ── decide: reject ────────────────────────────────────────────────────── */

func TestRejectingAProjectRequestProvisionsNothing(t *testing.T) {
	pool := newProjectRequestPool(t)
	handler := newProjectRequestHandler(t, pool)
	const requesterID int64 = 90201
	const operatorID int64 = 90202
	seedUser(t, pool, requesterID, "requester-reject@example.com")
	seedUser(t, pool, operatorID, "operator-reject@example.com")
	requester := auth.User{ID: fmt.Sprint(requesterID), UserID: fmt.Sprint(requesterID)}
	operator := auth.User{ID: fmt.Sprint(operatorID), UserID: fmt.Sprint(operatorID)}

	const projectName = "Rejected Skunkworks"
	before := countProjectsNamed(t, pool, projectName)
	created := doJSON(t, projectRequestRouter(handler, requester), http.MethodPost, projectRequestCreateURL,
		map[string]string{"name": projectName, "description": "Speculative."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdRow struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	decision := doJSON(t, projectRequestRouter(handler, operator), http.MethodPut, projectRequestDecideURL,
		map[string]any{"id": createdRow.ID, "status": "rejected", "rejection_comment": "Not this quarter."})
	if decision.Code != http.StatusOK {
		t.Fatalf("reject: status = %d, body = %s", decision.Code, decision.Body.String())
	}
	var decided struct {
		Status           string `json:"status"`
		RejectionComment string `json:"rejection_comment"`
		CreatedProjectID *int64 `json:"created_project_id"`
	}
	if err := json.Unmarshal(decision.Body.Bytes(), &decided); err != nil {
		t.Fatalf("decode decision response: %v", err)
	}
	if decided.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", decided.Status)
	}
	if decided.RejectionComment != "Not this quarter." {
		t.Errorf("rejection_comment = %q, want the operator's reason", decided.RejectionComment)
	}
	if decided.CreatedProjectID != nil {
		t.Errorf("created_project_id = %v, want absent on a rejection", *decided.CreatedProjectID)
	}
	if after := countProjectsNamed(t, pool, projectName); after != before {
		t.Errorf("projects named %q: before=%d after=%d, a rejection must provision nothing", projectName, before, after)
	}
}

/* ── decide: cannot provision twice ───────────────────────────────────── */

func TestApprovingAProjectRequestTwiceProvisionsOnlyOnce(t *testing.T) {
	pool := newProjectRequestPool(t)
	handler := newProjectRequestHandler(t, pool)
	const requesterID int64 = 90301
	const operatorID int64 = 90302
	seedUser(t, pool, requesterID, "requester-twice@example.com")
	seedUser(t, pool, operatorID, "operator-twice@example.com")
	requester := auth.User{ID: fmt.Sprint(requesterID), UserID: fmt.Sprint(requesterID)}
	operator := auth.User{ID: fmt.Sprint(operatorID), UserID: fmt.Sprint(operatorID)}

	const projectName = "Double Approved Project"
	created := doJSON(t, projectRequestRouter(handler, requester), http.MethodPost, projectRequestCreateURL,
		map[string]string{"name": projectName, "description": "Once, please."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdRow struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	operatorRouter := projectRequestRouter(handler, operator)
	first := doJSON(t, operatorRouter, http.MethodPut, projectRequestDecideURL,
		map[string]any{"id": createdRow.ID, "status": "approved"})
	if first.Code != http.StatusOK {
		t.Fatalf("first approve: status = %d, body = %s", first.Code, first.Body.String())
	}

	second := doJSON(t, operatorRouter, http.MethodPut, projectRequestDecideURL,
		map[string]any{"id": createdRow.ID, "status": "approved"})
	if second.Code != http.StatusConflict {
		t.Fatalf("second approve: status = %d, want 409 (body = %s)", second.Code, second.Body.String())
	}

	if count := countProjectsNamed(t, pool, projectName); count != 1 {
		t.Fatalf("projects named %q: got %d, want exactly 1 — a second decision must not provision again", projectName, count)
	}
}
