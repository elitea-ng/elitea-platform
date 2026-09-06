package social_test

// THE BUG, AT THE SURFACE THE SPA READS.
//
// `GET /social/author` answered `"personal_project_id": ""` for every account a
// fresh deployment created, and kept answering it. Nothing in this service
// created the `project_user_<uid>` project the resolver looks for, so the
// value could never change. apps/elitea-web reads "" as "no personal project
// yet": routes/-guards/indexRoute.ts redirects to `/onboarding`, and that
// screen polls THIS endpoint every five seconds waiting for a project nothing
// was going to provision. Every new user was stuck there.
//
// This test is written against the endpoint rather than against the ensurer, so
// it fails if the wiring is dropped — which is the half that was missing, not
// the algorithm. Remove `WithPersonalProjectEnsurer` from
// internal/api/router.go and the assertion below goes red.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	personalProjectSocialURLEnv    = "ELITEA_TEST_DATABASE_URL"
	personalProjectSocialBootstrap = "../../../infra/db/migrations/001_initial.sql"
)

// personalProjectSocialTemplate names the template database the tests here
// copy. The provisioner applies the ledgered TENANT history, which
// internal/infra/db.RunMigrations (what the other integration tests in this
// package use) does not install — so this suite builds the same template
// internal/application/projectprovisioning's own suite does.
//
// BUILT ON FIRST USE, NOT IN TestMain. A TestMain runs for the whole package,
// so a template that failed to build there would `os.Exit(1)` before m.Run()
// and take handler_test.go, authors_test.go and feedback_test.go down with it
// — three suites that touch no database at all. Building it lazily keeps the
// failure inside the tests that actually need it.
var (
	personalProjectSocialTemplateOnce sync.Once
	personalProjectSocialTemplate     string
	personalProjectSocialTemplateErr  error
)

// personalProjectSocialTemplateName builds the template once per package run
// and returns the same answer — or the same error — to every later caller.
func personalProjectSocialTemplateName(databaseURL string) (string, error) {
	personalProjectSocialTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(personalProjectSocialBootstrap)
		if err != nil {
			personalProjectSocialTemplateErr = fmt.Errorf("read bootstrap schema: %w", err)
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		adminPool, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			personalProjectSocialTemplateErr = fmt.Errorf("open admin pool: %w", err)
			return
		}
		defer adminPool.Close()
		personalProjectSocialTemplate, personalProjectSocialTemplateErr = dbtest.EnsureTemplate(
			ctx, adminPool, dbtest.Spec{
				Files:   platformmigrations.Files,
				Seed:    string(bootstrap),
				Tenants: []int64{1},
			})
	})
	return personalProjectSocialTemplate, personalProjectSocialTemplateErr
}

func personalProjectSocialDatabaseURL() string {
	databaseURL := os.Getenv(personalProjectSocialURLEnv)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	return databaseURL
}

func TestGetAuthorProvisionsTheMissingPersonalProject(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "stuck-on-onboarding@autotest.local", "Newcomer")

	// A wait long enough that the ensure this request starts can finish inside
	// it. Production uses three seconds; see WithPersonalProjectWait for why a
	// test must not.
	routes := handler.NewHandler(pool,
		handler.WithPersonalProjectEnsurer(newAuthorEnsurer(t, pool)),
		handler.WithPersonalProjectWait(120*time.Second),
	).Routes()

	author := func() struct {
		ID                string `json:"id"`
		PersonalProjectID string `json:"personal_project_id"`
	} {
		request := httptest.NewRequest(http.MethodGet, "/author/", nil)
		request = request.WithContext(auth.ContextWithUser(request.Context(),
			auth.User{ID: strconv.FormatInt(userID, 10), Email: "stuck-on-onboarding@autotest.local"}))
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /author/ status = %d (body %s)", recorder.Code, recorder.Body.String())
		}
		var decoded struct {
			ID                string `json:"id"`
			PersonalProjectID string `json:"personal_project_id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode author response %s: %v", recorder.Body.String(), err)
		}
		return decoded
	}

	// THE FIRST READ REPORTS THE PROJECT (F2).
	//
	// It used to answer the "honest" "" — provisioning had only just been asked
	// for — and the SPA reads "" as "no personal project yet":
	// routes/-guards/indexRoute.ts redirects to `/onboarding`, a screen that
	// says "about 5 minutes". A reload seconds later went straight to chat,
	// because the project the FIRST request provisioned was already there. The
	// user was sent to a waiting room for work this very request had done.
	//
	// GetAuthor now waits a bounded moment for the attempt IT started and
	// re-resolves. The wait never cancels the provisioning, so a machine slower
	// than the bound degrades to the previous behaviour instead of hanging —
	// which the sibling test below is what measures.
	resolved := author().PersonalProjectID
	if resolved == "" {
		t.Fatal("the first GET /social/author still reported no personal project, " +
			"so a first-time user is sent to /onboarding for a project this very " +
			"request provisioned")
	}

	// And it names the caller's OWN project — the resolver's first branch is
	// membership-checked precisely because this value is used as an
	// authorization scope (issue #166/#167).
	var name string
	var ownerID int64
	var created bool
	projectID, err := strconv.ParseInt(resolved, 10, 64)
	if err != nil {
		t.Fatalf("personal_project_id %q is not an id: %v", resolved, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT name, owner_id, create_success FROM centry.project WHERE id = $1`, projectID,
	).Scan(&name, &ownerID, &created); err != nil {
		t.Fatalf("read the reported project %d: %v", projectID, err)
	}
	if want := personalproject.Name(userID); name != want {
		t.Fatalf("the reported project is named %q, want %q", name, want)
	}
	if ownerID != userID || !created {
		t.Fatalf("owner = %d (want %d), create_success = %v (want true)", ownerID, userID, created)
	}
}

// THE TWO REQUESTS THE SPA ACTUALLY SENDS AT BOOT.
//
// apps/elitea-web asks for `/social/author` twice, inside the same second. Both
// find no personal project, so both ask for one. The first took the attempt and
// waited for it; the second was told "another attempt already owns this user",
// had nothing to wait for, and answered `personal_project_id: ""`. One boot,
// two contradictory answers, on the field routes/-guards/indexRoute.ts routes
// on — so a first-time user was sent to `/onboarding` or to the product
// depending on which of the two the SPA read last.
//
// EnsureStarted now hands a concurrent caller for the SAME user the running
// attempt's channel, so both requests wait for the one provisioning run and
// both report the one project.
func TestTwoConcurrentFirstReadsAgreeOnThePersonalProject(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "two-boot-requests@autotest.local", "Twin")

	routes := handler.NewHandler(pool,
		handler.WithPersonalProjectEnsurer(newAuthorEnsurer(t, pool)),
		handler.WithPersonalProjectWait(120*time.Second),
	).Routes()

	read := func() string {
		request := httptest.NewRequest(http.MethodGet, "/author/", nil)
		request = request.WithContext(auth.ContextWithUser(request.Context(),
			auth.User{ID: strconv.FormatInt(userID, 10), Email: "two-boot-requests@autotest.local"}))
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			return fmt.Sprintf("status %d: %s", recorder.Code, recorder.Body.String())
		}
		var decoded struct {
			PersonalProjectID string `json:"personal_project_id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			return fmt.Sprintf("undecodable body %s: %v", recorder.Body.String(), err)
		}
		return decoded.PersonalProjectID
	}

	// Sent together, as the browser sends them. The slot budget is one, so the
	// second request cannot start an attempt of its own: it either joins the
	// first or it is refused, and this is what tells the two apart.
	answers := make([]string, 2)
	var boot sync.WaitGroup
	begin := make(chan struct{})
	for index := range answers {
		boot.Add(1)
		go func() {
			defer boot.Done()
			<-begin
			answers[index] = read()
		}()
	}
	close(begin)
	boot.Wait()

	for index, answer := range answers {
		if answer == "" {
			t.Fatalf("request %d answered no personal project while its twin answered %q; "+
				"the SPA routes on this field, so one boot sends the user to the product "+
				"and the other to /onboarding", index, answers[1-index])
		}
		// A failed request is reported through this same slice, because a test
		// goroutine may not call Fatalf. An answer that is not an id is one.
		if _, err := strconv.ParseInt(answer, 10, 64); err != nil {
			t.Fatalf("request %d did not answer an id: %s", index, answer)
		}
	}
	if answers[0] != answers[1] {
		t.Fatalf("the two boot requests named different personal projects: %q and %q",
			answers[0], answers[1])
	}

	// One project, not two: the joining request must not have provisioned a
	// second `project_user_<uid>` beside the first.
	var projects int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM centry.project WHERE name = $1`, personalproject.Name(userID),
	).Scan(&projects); err != nil {
		t.Fatalf("count the personal projects of user %d: %v", userID, err)
	}
	if projects != 1 {
		t.Fatalf("%d projects are named %s, want 1", projects, personalproject.Name(userID))
	}
}

// The endpoint must keep working when the composition could not build an
// ensurer — a pool-less deployment, which is the same gate the project-create
// route uses. The handler holds a nil ensurer and answers as it always did.
func TestGetAuthorWithoutAnEnsurerStillAnswers(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "no-ensurer@autotest.local", "Plain")

	request := httptest.NewRequest(http.MethodGet, "/author/", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: strconv.FormatInt(userID, 10), Email: "no-ensurer@autotest.local"}))
	recorder := httptest.NewRecorder()
	handler.NewHandler(pool).Routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
}

/* ── fixture ───────────────────────────────────────────────────────────── */

func newAuthorEnsurer(t *testing.T, pool *pgxpool.Pool) *personalproject.Ensurer {
	t.Helper()
	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	ensurer, err := personalproject.NewEnsurer(pool, provisioner)
	if err != nil {
		t.Fatalf("build ensurer: %v", err)
	}
	return ensurer
}

func seedAuthorUser(t *testing.T, pool *pgxpool.Pool, email, name string) int64 {
	t.Helper()
	var userID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $2) RETURNING id`,
		email, name,
	).Scan(&userID); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return userID
}

// newPersonalProjectSocialPool builds an isolated database holding the
// bootstrap schema and the full ledgered corpus — the provisioner applies the
// tenant history, so a hand-built fixture could not run it.
func newPersonalProjectSocialPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := personalProjectSocialDatabaseURL()
	if databaseURL == "" {
		t.Skipf("set %s to run the personal project author-endpoint test", personalProjectSocialURLEnv)
	}
	template, err := personalProjectSocialTemplateName(databaseURL)
	if err != nil {
		t.Fatalf("build the personal project template: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_social_pp_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, template, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, parseErr := pgxpool.ParseConfig(databaseURL)
	if parseErr != nil {
		t.Fatalf("parse %s: %v", personalProjectSocialURLEnv, parseErr)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, poolErr := pgxpool.NewWithConfig(ctx, config)
	if poolErr != nil {
		t.Fatalf("open isolated pool: %v", poolErr)
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

// THE FALLBACK, WHICH IS THE HALF THAT KEEPS THE ENDPOINT HONEST (F2).
//
// The bounded wait must bound the WAIT and never the WORK. With a wait this
// short the first read cannot see the finished project, so it answers "" — the
// behaviour that shipped before — and the provisioning it started keeps running
// on its own deadline. A later poll then resolves the id.
//
// Without this case, "the first read answers the id" could be satisfied by an
// implementation that blocks the request until the tenant corpus is applied,
// which holds a poll open for minutes; or by one that cancels the attempt when
// the wait expires, which leaves a half-built project for the next attempt to
// repair on every single first login.
func TestGetAuthorFallsBackToThePollWhenTheWaitExpires(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "slow-provisioning@autotest.local", "Patient")

	routes := handler.NewHandler(pool,
		handler.WithPersonalProjectEnsurer(newAuthorEnsurer(t, pool)),
		handler.WithPersonalProjectWait(time.Nanosecond),
	).Routes()

	read := func() string {
		request := httptest.NewRequest(http.MethodGet, "/author/", nil)
		request = request.WithContext(auth.ContextWithUser(request.Context(),
			auth.User{ID: strconv.FormatInt(userID, 10), Email: "slow-provisioning@autotest.local"}))
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /author/ status = %d (body %s)", recorder.Code, recorder.Body.String())
		}
		var decoded struct {
			PersonalProjectID string `json:"personal_project_id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode author response %s: %v", recorder.Body.String(), err)
		}
		return decoded.PersonalProjectID
	}

	if first := read(); first != "" {
		t.Fatalf("the first read answered %q with a one-nanosecond wait; the "+
			"request is waiting for the whole provisioning run, not for a bounded moment",
			first)
	}

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if read() != "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the abandoned wait also abandoned the work: no later poll ever " +
		"reported a personal project")
}
