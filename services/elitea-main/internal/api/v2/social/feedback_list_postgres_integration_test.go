package social_test

// The project feedback list and create routes, over real PostgreSQL and the
// real RBAC resolver. Feedback lives in the shared centry.social_feedbacks
// table with a nullable project_id; the visibility rule is:
//
//	member AND ((row.project_id = project AND (not own-only OR row.user_id = caller))
//	            OR (row.project_id IS NULL AND row.user_id = caller))
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const feedbackListPermission = "models.social.feedbacks.list"

// Users on top of prepareCurrentFeedbackDatabase (41 admin, 42 editor, 43
// viewer of project 7; 44 holds no feedback grant; 48 admin of project 8):
// 50 platform super_admin with no project role, 51 in no project, 52 a second
// viewer of project 7, 53 a viewer of project 8.
func prepareFeedbackListDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	prepareCurrentFeedbackDatabase(t, pool)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__user (id, email, name) VALUES
    (50, 'platform-admin@elitea.example', 'Platform Admin'),
    (51, 'no-project@elitea.example', 'No Project'),
    (52, 'viewer-two@elitea.example', 'Viewer Two'),
    (53, 'viewer-eight@elitea.example', 'Viewer Eight');
INSERT INTO public.auth_core__role (id, name, mode) VALUES (900, 'super_admin', 'administration');
INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (50, 900);
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (202, 8, 'viewer');
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES
    (7, 101, 'models.social.feedbacks.list'),
    (7, 102, 'models.social.feedbacks.list'),
    (7, 103, 'models.social.feedbacks.list'),
    (8, 201, 'models.social.feedbacks.list'),
    (8, 202, 'models.social.feedbacks.list'),
    (8, 202, 'models.social.feedbacks.create');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (7, 52, 103),
    (8, 53, 202);`); err != nil {
		t.Fatalf("prepare feedback list database: %v", err)
	}
}

type feedbackGrantAll struct{}

func (feedbackGrantAll) ResolvePermissions(
	_ context.Context, user auth.User, _ string, _ string,
) (auth.PermissionResolution, error) {
	id, _ := strconv.ParseInt(user.ID, 10, 64)
	return auth.PermissionResolution{
		UserID:      id,
		Permissions: []string{feedbackListPermission, handler.CurrentFeedbackCreatePermission},
	}, nil
}

// feedbackRouter mounts the real Social routes behind an identity stub: the
// X-Test-Actor header is the caller, and no header means unauthenticated.
func feedbackRouter(pool *pgxpool.Pool, options ...handler.Option) http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id := r.Header.Get("X-Test-Actor"); id != "" {
				r = r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: id, UserID: id}))
			}
			next.ServeHTTP(w, r)
		})
	})
	router.Mount("/api/v2/social", handler.NewHandler(pool, options...).Routes())
	return router
}

func feedbackRealRBACRouter(pool *pgxpool.Pool) http.Handler {
	return feedbackRouter(pool, handler.WithPermissionResolver(legacyrbac.NewPostgresResolver(pool)))
}

func feedbackDo(router http.Handler, method, actor, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		request.Header.Set("X-Test-Actor", actor)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

type feedbackListBody struct {
	Total int64 `json:"total"`
	Rows  []struct {
		ID          int64   `json:"id"`
		UserID      int64   `json:"user_id"`
		ProjectID   *int64  `json:"project_id"`
		Referrer    *string `json:"referrer"`
		Description string  `json:"description"`
		Rating      int     `json:"rating"`
		UserAgent   *string `json:"user_agent"`
		CreatedAt   string  `json:"created_at"`
	} `json:"rows"`
}

func feedbackList(t *testing.T, router http.Handler, actor, target string) feedbackListBody {
	t.Helper()
	recorder := feedbackDo(router, http.MethodGet, actor, target, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s as %s: status %d body %s", target, actor, recorder.Code, recorder.Body)
	}
	var body feedbackListBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body, err)
	}
	return body
}

func feedbackDescriptions(body feedbackListBody) string {
	descriptions := make([]string, 0, len(body.Rows))
	for _, row := range body.Rows {
		descriptions = append(descriptions, row.Description)
	}
	return strings.Join(descriptions, ",")
}

func seedFeedbackRow(t *testing.T, pool *pgxpool.Pool, userID int, projectID any, description string, createdAt string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.social_feedbacks (user_id, project_id, description, rating, created_at)
VALUES ($1, $2, $3, 3, $4::timestamp)`, userID, projectID, description, createdAt); err != nil {
		t.Fatalf("seed feedback %q: %v", description, err)
	}
}

func feedbackRowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM centry.social_feedbacks`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestFeedbackWriteThenListRoundTrip(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	router := feedbackRealRBACRouter(pool)

	create := httptest.NewRequest(http.MethodPost, "/api/v2/social/feedbacks/default/7",
		strings.NewReader(`{"description":"round trip","rating":4,"referrer":"https://attacker.invalid"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-Test-Actor", "42")
	create.Header.Set("Referer", "https://elitea.example/app")
	create.Header.Set("User-Agent", "EliteaUI/test")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, create)
	if recorder.Code != http.StatusCreated || recorder.Body.String() != "{\"id\":1}\n" {
		t.Fatalf("create: status %d body %q", recorder.Code, recorder.Body)
	}

	// The alias path writes too.
	if r := feedbackDo(router, http.MethodPost, "43", "/api/v2/social/feedbacks/7",
		`{"description":"via alias","rating":0}`); r.Code != http.StatusCreated {
		t.Fatalf("alias create: %d %s", r.Code, r.Body)
	}

	for _, target := range []string{"/api/v2/social/feedbacks/default/7", "/api/v2/social/feedbacks/7"} {
		body := feedbackList(t, router, "41", target)
		if body.Total != 2 || len(body.Rows) != 2 {
			t.Fatalf("%s: %+v", target, body)
		}
		row := body.Rows[0]
		if row.ID != 1 || row.UserID != 42 || row.ProjectID == nil || *row.ProjectID != 7 ||
			row.Description != "round trip" || row.Rating != 4 ||
			row.Referrer == nil || *row.Referrer != "https://elitea.example/app" ||
			row.UserAgent == nil || *row.UserAgent != "EliteaUI/test" {
			t.Fatalf("%s: row %+v", target, row)
		}
		if _, err := time.Parse("2006-01-02T15:04:05.999999999", row.CreatedAt); err != nil {
			t.Fatalf("created_at %q is not ISO-8601: %v", row.CreatedAt, err)
		}
	}

	var projectID int
	if err := pool.QueryRow(context.Background(),
		`SELECT project_id FROM centry.social_feedbacks WHERE id = 1`).Scan(&projectID); err != nil || projectID != 7 {
		t.Fatalf("stored project_id=%d err=%v", projectID, err)
	}
}

func TestFeedbackAuthorizationMatrix(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	router := feedbackRealRBACRouter(pool)
	seedFeedbackRow(t, pool, 48, 8, "eight-canary", "2026-01-01 00:00:00")

	for _, path := range []string{"/api/v2/social/feedbacks/default/7", "/api/v2/social/feedbacks/7"} {
		for _, test := range []struct {
			name       string
			actor      string
			wantList   int
			wantCreate int
		}{
			{"owner admin", "41", http.StatusOK, http.StatusCreated},
			{"member viewer", "43", http.StatusOK, http.StatusCreated},
			{"foreign project actor", "48", http.StatusForbidden, http.StatusForbidden},
			{"user with no project", "51", http.StatusForbidden, http.StatusForbidden},
			{"member without the permission", "44", http.StatusForbidden, http.StatusForbidden},
			{"unauthenticated", "", http.StatusUnauthorized, http.StatusUnauthorized},
		} {
			t.Run(test.name+" "+path, func(t *testing.T) {
				get := feedbackDo(router, http.MethodGet, test.actor, path, "")
				if get.Code != test.wantList {
					t.Fatalf("GET status %d, want %d (%s)", get.Code, test.wantList, get.Body)
				}
				if test.wantList != http.StatusOK && strings.Contains(get.Body.String(), "canary") {
					t.Fatalf("refused GET leaked a row: %s", get.Body)
				}
				before := feedbackRowCount(t, pool)
				post := feedbackDo(router, http.MethodPost, test.actor, path,
					`{"description":"matrix write","rating":5}`)
				if post.Code != test.wantCreate {
					t.Fatalf("POST status %d, want %d (%s)", post.Code, test.wantCreate, post.Body)
				}
				wantRows := before
				if test.wantCreate == http.StatusCreated {
					wantRows++
				}
				if got := feedbackRowCount(t, pool); got != wantRows {
					t.Fatalf("rows %d -> %d, want %d", before, got, wantRows)
				}
			})
		}
	}

	// The foreign project's row never shows in project 7, whoever asks.
	for _, actor := range []string{"41", "43"} {
		if body := feedbackList(t, router, actor, "/api/v2/social/feedbacks/default/7"); strings.Contains(feedbackDescriptions(body), "eight-canary") {
			t.Fatalf("project 8 row listed in project 7 for %s", actor)
		}
	}
	// Refused GETs on a path that is not a canonical project id.
	if r := feedbackDo(router, http.MethodGet, "41", "/api/v2/social/feedbacks/default/007", ""); r.Code != http.StatusForbidden && r.Code != http.StatusBadRequest {
		t.Fatalf("non-canonical project id: %d", r.Code)
	}
}

// A platform super_admin passes the in-statement membership check without a
// project role. With the real resolver the default-mode permission gate still
// asks for a project grant (as on every default-mode route), so the HTTP check
// uses a resolver that grants and the SQL decision is what is under test.
func TestFeedbackPlatformAdministratorReadsTheProjectsRows(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	seedFeedbackRow(t, pool, 42, 7, "seven", "2026-01-01 00:00:00")
	seedFeedbackRow(t, pool, 48, 8, "eight", "2026-01-02 00:00:00")
	seedFeedbackRow(t, pool, 42, nil, "legacy-of-42", "2026-01-03 00:00:00")
	router := feedbackRouter(pool, handler.WithPermissionResolver(feedbackGrantAll{}))

	body := feedbackList(t, router, "50", "/api/v2/social/feedbacks/default/7")
	if body.Total != 1 || feedbackDescriptions(body) != "seven" {
		t.Fatalf("administrator listing: %+v", body)
	}
	// An administrator is a member of every id, but a project that does not
	// exist cannot receive a row.
	if r := feedbackDo(router, http.MethodPost, "50", "/api/v2/social/feedbacks/default/999999",
		`{"description":"ghost","rating":1}`); r.Code != http.StatusNotFound {
		t.Fatalf("absent project: %d %s", r.Code, r.Body)
	}
	if r := feedbackDo(router, http.MethodPost, "50", "/api/v2/social/feedbacks/default/7",
		`{"description":"by admin","rating":1}`); r.Code != http.StatusCreated {
		t.Fatalf("administrator create: %d %s", r.Code, r.Body)
	}
}

// The statement itself refuses a non-member: a membership revoked between the
// HTTP gate and the query cannot be used to read or write.
func TestFeedbackStatementsRefuseANonMemberThemselves(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	seedFeedbackRow(t, pool, 42, 7, "seven", "2026-01-01 00:00:00")
	repository, err := dbrepos.NewCurrentSocialFeedbacksRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page := dbrepos.FeedbackPage{Limit: 10, SortBy: "id"}

	for _, caller := range []int64{48, 51} {
		if _, err := repository.ListCurrentFeedback(ctx, caller, 7, false, page); !errors.Is(err, dbrepos.ErrSocialFeedbackForbidden) {
			t.Fatalf("caller %d list err=%v", caller, err)
		}
		before := feedbackRowCount(t, pool)
		if _, err := repository.CreateCurrentFeedback(ctx, caller, 7, "x", 1, nil, ""); !errors.Is(err, dbrepos.ErrSocialFeedbackForbidden) {
			t.Fatalf("caller %d create err=%v", caller, err)
		}
		if after := feedbackRowCount(t, pool); after != before {
			t.Fatalf("a refused insert wrote a row: %d -> %d", before, after)
		}
	}

	// An administrator is a member of every id, yet cannot write to a project
	// that has no row.
	if _, err := repository.CreateCurrentFeedback(ctx, 50, 999999, "x", 1, nil, ""); !errors.Is(err, dbrepos.ErrSocialFeedbackForbidden) {
		t.Fatalf("administrator create into an absent project err=%v", err)
	}

	// Revoke the membership after "the gate": the same member is now refused.
	if _, err := repository.ListCurrentFeedback(ctx, 43, 7, false, page); err != nil {
		t.Fatalf("member list: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__project_user_role WHERE user_id = 43`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListCurrentFeedback(ctx, 43, 7, false, page); !errors.Is(err, dbrepos.ErrSocialFeedbackForbidden) {
		t.Fatalf("revoked member list err=%v", err)
	}
	if _, err := repository.CreateCurrentFeedback(ctx, 43, 7, "x", 1, nil, ""); !errors.Is(err, dbrepos.ErrSocialFeedbackForbidden) {
		t.Fatalf("revoked member create err=%v", err)
	}
}

func TestFeedbackIsolationAndLegacyRows(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	seedFeedbackRow(t, pool, 42, 7, "seven-by-42", "2026-01-01 00:00:00")
	seedFeedbackRow(t, pool, 41, 7, "seven-by-41", "2026-01-02 00:00:00")
	seedFeedbackRow(t, pool, 48, 8, "eight-by-48", "2026-01-03 00:00:00")
	seedFeedbackRow(t, pool, 41, nil, "legacy-41", "2026-01-04 00:00:00")
	seedFeedbackRow(t, pool, 42, nil, "legacy-42", "2026-01-05 00:00:00")
	seedFeedbackRow(t, pool, 48, nil, "legacy-48", "2026-01-06 00:00:00")
	router := feedbackRealRBACRouter(pool)

	for _, test := range []struct {
		actor, target, want string
		total               int64
	}{
		// A project's rows plus the caller's own legacy rows, nothing else.
		{"41", "/api/v2/social/feedbacks/default/7", "seven-by-42,seven-by-41,legacy-41", 3},
		{"42", "/api/v2/social/feedbacks/default/7", "seven-by-42,seven-by-41,legacy-42", 3},
		{"43", "/api/v2/social/feedbacks/default/7", "seven-by-42,seven-by-41", 2},
		{"48", "/api/v2/social/feedbacks/default/8", "eight-by-48,legacy-48", 2},
		{"53", "/api/v2/social/feedbacks/default/8", "eight-by-48", 1},
	} {
		body := feedbackList(t, router, test.actor, test.target)
		if got := feedbackDescriptions(body); got != test.want || body.Total != test.total {
			t.Fatalf("%s %s: got %q total %d, want %q total %d", test.actor, test.target, got, body.Total, test.want, test.total)
		}
	}
}

func TestFeedbackPublicProjectListsOnlyTheCallersOwnRows(t *testing.T) {
	t.Setenv("PUBLIC_PROJECT_ID", "8")
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	seedFeedbackRow(t, pool, 48, 8, "public-by-48", "2026-01-01 00:00:00")
	seedFeedbackRow(t, pool, 53, 8, "public-by-53", "2026-01-02 00:00:00")
	seedFeedbackRow(t, pool, 53, nil, "legacy-53", "2026-01-03 00:00:00")
	router := feedbackRealRBACRouter(pool)

	for actor, want := range map[string]string{
		"48": "public-by-48",
		"53": "public-by-53,legacy-53",
	} {
		body := feedbackList(t, router, actor, "/api/v2/social/feedbacks/default/8")
		if got := feedbackDescriptions(body); got != want {
			t.Fatalf("public project as %s: got %q, want %q", actor, got, want)
		}
	}
}

func TestFeedbackEmptyProjectListsNothing(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	router := feedbackRealRBACRouter(pool)

	recorder := feedbackDo(router, http.MethodGet, "41", "/api/v2/social/feedbacks/default/7", "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"total\":0,\"rows\":[]}\n" ||
		recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d body %q", recorder.Code, recorder.Body)
	}
}

func TestFeedbackPaginationSortAndBounds(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	// ids 1..5; created_at runs against the id order for rows 4 and 5.
	for i, created := range []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-05", "2026-01-04"} {
		seedFeedbackRow(t, pool, 42, 7, fmt.Sprintf("row-%d", i+1), created+" 00:00:00")
	}
	router := feedbackRealRBACRouter(pool)
	base := "/api/v2/social/feedbacks/default/7"

	for _, test := range []struct {
		query, want string
	}{
		{"", "row-1,row-2,row-3,row-4,row-5"},
		{"?limit=2", "row-1,row-2"},
		{"?limit=2&offset=1", "row-2,row-3"},
		{"?limit=2&offset=4", "row-5"},
		{"?offset=5", ""},
		{"?sort_order=desc", "row-5,row-4,row-3,row-2,row-1"},
		{"?sort_by=created_at", "row-1,row-2,row-3,row-5,row-4"},
		{"?sort_by=created_at&sort_order=desc&limit=2", "row-4,row-5"},
		{"?limit=200&offset=100000", ""},
	} {
		body := feedbackList(t, router, "41", base+test.query)
		if got := feedbackDescriptions(body); got != test.want || body.Total != 5 {
			t.Fatalf("%q: got %q total %d, want %q total 5", test.query, got, body.Total, test.want)
		}
	}

	for _, query := range []string{
		"?limit=0", "?limit=201", "?limit=abc", "?offset=-1", "?offset=100001",
		"?sort_by=user_id", "?sort_by=id;drop", "?sort_order=up", "?unknown=1", "?limit=1&limit=2",
	} {
		recorder := feedbackDo(router, http.MethodGet, "41", base+query, "")
		if recorder.Code != http.StatusBadRequest || recorder.Body.String() != "{\"error\":\"invalid request\"}\n" {
			t.Fatalf("%q: status %d body %q", query, recorder.Code, recorder.Body)
		}
	}
}

func TestFeedbackRoutesFailClosedWithoutAResolver(t *testing.T) {
	pool := newCurrentFeedbackPostgresPool(t)
	prepareFeedbackListDatabase(t, pool)
	router := feedbackRouter(pool)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		recorder := feedbackDo(router, method, "41", "/api/v2/social/feedbacks/default/7",
			`{"description":"x","rating":1}`)
		if recorder.Code != http.StatusServiceUnavailable ||
			recorder.Body.String() != "{\"error\":\"feedback authorization unavailable\"}\n" {
			t.Fatalf("%s: status %d body %q", method, recorder.Code, recorder.Body)
		}
		if unauthenticated := feedbackDo(router, method, "", "/api/v2/social/feedbacks/default/7", ""); unauthenticated.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: %d", method, unauthenticated.Code)
		}
	}
	if feedbackRowCount(t, pool) != 0 {
		t.Fatal("an unavailable route wrote a row")
	}
}
