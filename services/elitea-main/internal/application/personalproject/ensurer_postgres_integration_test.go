package personalproject_test

// What these tests exist to catch.
//
// The defect this package closes is not "a function returned the wrong value" —
// it is "nothing ran at all". `GET /social/author` answered
// `personal_project_id: ""` for every account a fresh deployment created,
// because no code path in this service ever created a `project_user_<uid>`
// project. Asserting that Ensure returns a non-zero id would not have caught
// that, and would not catch its recurrence: the id has to name a project the
// AUTHOR RESOLVER can find, which means the row, its `create_success` marker,
// its tenant schema and the owner's project-role assignment all have to exist.
//
// So every case below reads the database back, and the central one runs the
// resolver's own SQL — the exact query internal/api/v2/social/handler.go's
// resolvePersonalProjectID uses — against the result.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	databaseURLEnv  = "ELITEA_TEST_DATABASE_URL"
	bootstrapSchema = "../../infra/db/migrations/001_initial.sql"
)

// personalProjectTemplate names the template database every test here copies.
// Same mechanism as internal/application/projectprovisioning's own suite: the
// ledgered corpus runs once, into a template, and each test gets a private
// database from CREATE DATABASE ... TEMPLATE.
var personalProjectTemplate string

func TestMain(m *testing.M) {
	databaseURL := personalProjectDatabaseURL()
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(bootstrapSchema)
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
		fmt.Fprintf(os.Stderr, "build personal project template: %v\n", err)
		os.Exit(1)
	}
	personalProjectTemplate = templateName
	os.Exit(m.Run())
}

func personalProjectDatabaseURL() string {
	databaseURL := os.Getenv(databaseURLEnv)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	return databaseURL
}

// THE REGRESSION TEST FOR THE BUG ITSELF.
//
// A user who holds no project at all — the state every account is in on a fresh
// deployment — gets a personal project, and the author resolver finds it.
// Before this package, the second half of that sentence was false forever.
func TestEnsureCreatesAPersonalProjectTheAuthorResolverFinds(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	userID := seedUser(t, pool, "newcomer@autotest.local", "Newcomer")

	// The precondition, asserted rather than assumed: this is the answer the
	// SPA gets today, and the reason it parks the browser on /onboarding.
	if resolved := resolveAuthorPersonalProjectID(ctx, t, pool, userID); resolved != 0 {
		t.Fatalf("fixture user already resolves project %d; the test proves nothing", resolved)
	}

	projectID, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if projectID <= 0 {
		t.Fatalf("Ensure returned project id %d, want a positive id", projectID)
	}

	// The row, by the name both readers look it up under.
	var name string
	var created, suspended bool
	var ownerID int64
	if err := pool.QueryRow(ctx,
		`SELECT name, owner_id, create_success, suspended FROM centry.project WHERE id = $1`,
		projectID,
	).Scan(&name, &ownerID, &created, &suspended); err != nil {
		t.Fatalf("read the created project: %v", err)
	}
	if want := personalproject.Name(userID); name != want {
		t.Fatalf("project name = %q, want %q", name, want)
	}
	if ownerID != userID {
		t.Fatalf("project owner = %d, want %d", ownerID, userID)
	}
	if !created || suspended {
		t.Fatalf("create_success = %v, suspended = %v; want true/false", created, suspended)
	}

	// The tenant. A project row with no `p_<id>` schema is the one state
	// cmd/elitea-migrate refuses to run against, for the whole deployment.
	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		fmt.Sprintf("p_%d", projectID),
	).Scan(&schemaExists); err != nil {
		t.Fatal(err)
	}
	if !schemaExists {
		t.Fatalf("tenant schema p_%d was not created", projectID)
	}

	// The membership. The resolver's first branch is membership-checked, so a
	// project without it resolves to nothing — which is the whole bug again.
	var roles []string
	rows, err := pool.Query(ctx, `
SELECT role.name
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__project_role AS role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2`, projectID, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		t.Fatal("the owner holds no project role in their own personal project")
	}

	// And the answer the SPA actually reads.
	if resolved := resolveAuthorPersonalProjectID(ctx, t, pool, userID); resolved != projectID {
		t.Fatalf("the author resolver answered %d, want %d", resolved, projectID)
	}
}

// The onboarding screen polls `/social/author` every five seconds, and every
// poll that still resolves "" asks for provisioning again. A second Ensure must
// therefore converge on the SAME project rather than create a second one —
// centry.project has no unique index on `name`, so nothing else prevents it.
func TestEnsureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	userID := seedUser(t, pool, "repeat@autotest.local", "Repeat")

	first, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if first != second {
		t.Fatalf("second Ensure returned %d, want the first project %d", second, first)
	}

	var projects int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE name = $1`, personalproject.Name(userID),
	).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 1 {
		t.Fatalf("%d projects named %q, want 1", projects, personalproject.Name(userID))
	}
}

// A project row left behind with `create_success = false` is worse than no row
// at all: the resolver's first branch matches on the NAME, so its owner would
// be handed a project id no tenant schema backs, and no later attempt would
// ever replace it. pylon repairs the same state by deleting and recreating
// (fix_create_personal_projects).
func TestEnsureRepairsAnUnfinishedPersonalProject(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	userID := seedUser(t, pool, "halfway@autotest.local", "Halfway")

	var strandedID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO centry.project (name, owner_id, plugins, create_success)
		 VALUES ($1, $2, '{}', false) RETURNING id`,
		personalproject.Name(userID), userID,
	).Scan(&strandedID); err != nil {
		t.Fatalf("seed the unfinished project: %v", err)
	}

	repaired, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("Ensure over an unfinished project: %v", err)
	}
	if repaired == strandedID {
		t.Fatalf("Ensure kept the unfinished project %d", strandedID)
	}

	var survivors int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, strandedID).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors != 0 {
		t.Fatalf("the unfinished project %d survived the repair", strandedID)
	}
	if resolved := resolveAuthorPersonalProjectID(ctx, t, pool, userID); resolved != repaired {
		t.Fatalf("the author resolver answered %d, want the repaired project %d", resolved, repaired)
	}
}

// deprovisionOutcomeProvisioner runs the real provisioner and then reports the
// given outcome from Deprovision, so the ensurer's reaction to each delete
// failure is tested against a real half-created project.
type deprovisionOutcomeProvisioner struct {
	*projectprovisioning.Provisioner
	outcome error
}

func (p deprovisionOutcomeProvisioner) Deprovision(ctx context.Context, projectID int64, options ...projectprovisioning.DeprovisionOption) (projectprovisioning.Result, error) {
	result, err := p.Provisioner.Deprovision(ctx, projectID, options...)
	if err != nil {
		return result, err
	}
	return result, p.outcome
}

func seedUnfinishedProject(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO centry.project (name, owner_id, plugins, create_success)
		 VALUES ($1, $2, '{}', false) RETURNING id`,
		personalproject.Name(userID), userID,
	).Scan(&id); err != nil {
		t.Fatalf("seed the unfinished project: %v", err)
	}
	return id
}

func newOutcomeEnsurer(t *testing.T, pool *pgxpool.Pool, outcome error, logs *bytes.Buffer) *personalproject.Ensurer {
	t.Helper()
	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	ensurer, err := personalproject.NewEnsurer(pool,
		deprovisionOutcomeProvisioner{Provisioner: provisioner, outcome: outcome},
		personalproject.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatalf("build ensurer: %v", err)
	}
	return ensurer
}

// The row is gone, so a leftover PgVector database must not fail the repair.
func TestEnsureRepairContinuesWhenTheVectorStoreWasNotDropped(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	var logs bytes.Buffer
	ensurer := newOutcomeEnsurer(t, pool,
		fmt.Errorf("%w: database \"project_9\" remains", projectprovisioning.ErrVectorStoreNotDropped), &logs)
	userID := seedUser(t, pool, "leftover@autotest.local", "Leftover")
	strandedID := seedUnfinishedProject(ctx, t, pool, userID)

	repaired, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("Ensure must continue past ErrVectorStoreNotDropped: %v", err)
	}
	if repaired == 0 || repaired == strandedID {
		t.Fatalf("Ensure returned %d for stranded project %d", repaired, strandedID)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "PgVector database was not dropped") {
		t.Fatalf("the leftover was not logged at warn:\n%s", out)
	}
}

// Only a leftover PgVector database is tolerated. The same delete reporting a
// tenant schema or artifact bytes left behind as well fails the repair, as it
// did before the vector store existed.
func TestEnsureRepairFailsWhenAnythingBesidesTheVectorStoreWasLeft(t *testing.T) {
	vector := fmt.Errorf("%w: database \"project_9\" remains", projectprovisioning.ErrVectorStoreNotDropped)
	for name, outcome := range map[string]error{
		"vector store and tenant schema": errors.Join(projectprovisioning.ErrTenantSchemaNotRemoved, vector),
		"vector store and artifacts":     errors.Join(vector, projectprovisioning.ErrArtifactsNotRemoved),
		"all three":                      errors.Join(projectprovisioning.ErrTenantSchemaNotRemoved, projectprovisioning.ErrArtifactsNotRemoved, vector),
		"vector store and an unknown":    errors.Join(vector, errors.New("verify project removal")),
		"vector store and the vault":     errors.Join(vector, fmt.Errorf("%w: step project_secrets", projectprovisioning.ErrCleanupIncomplete)),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := newPersonalProjectPool(t)
			var logs bytes.Buffer
			ensurer := newOutcomeEnsurer(t, pool, outcome, &logs)
			userID := seedUser(t, pool, "multi-leftover@autotest.local", "Multi Leftover")
			seedUnfinishedProject(ctx, t, pool, userID)

			if _, err := ensurer.Ensure(ctx, userID); err == nil {
				t.Fatal("Ensure carried on past a leftover that is not only the vector store")
			}
		})
	}
	t.Run("vector store alone continues", func(t *testing.T) {
		ctx := context.Background()
		pool := newPersonalProjectPool(t)
		var logs bytes.Buffer
		ensurer := newOutcomeEnsurer(t, pool, errors.Join(vector), &logs)
		userID := seedUser(t, pool, "single-leftover@autotest.local", "Single Leftover")
		seedUnfinishedProject(ctx, t, pool, userID)

		if projectID, err := ensurer.Ensure(ctx, userID); err != nil || projectID == 0 {
			t.Fatalf("Ensure = %d, %v; want a repaired project", projectID, err)
		}
	})
}

// Active work is retryable and the repair cannot go on: the error says why.
func TestEnsureRepairReportsActiveWorkClearly(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	var logs bytes.Buffer
	refusing, err := personalproject.NewEnsurer(pool, activeWorkProvisioner{}, personalproject.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	userID := seedUser(t, pool, "busyrepair@autotest.local", "Busy Repair")
	seedUnfinishedProject(ctx, t, pool, userID)

	_, err = refusing.Ensure(ctx, userID)
	if !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("Ensure err = %v, want ErrProjectWorkActive", err)
	}
	if !strings.Contains(err.Error(), "active runs") {
		t.Fatalf("the error does not say what to do: %v", err)
	}
}

type activeWorkProvisioner struct{}

func (activeWorkProvisioner) Provision(context.Context, projectprovisioning.Request) (projectprovisioning.Result, error) {
	panic("provision must not run while the repair is blocked")
}

func (activeWorkProvisioner) Deprovision(context.Context, int64, ...projectprovisioning.DeprovisionOption) (projectprovisioning.Result, error) {
	return projectprovisioning.Result{}, projectprovisioning.ErrProjectWorkActive
}

// The three identities that must NOT be given a personal project, and the
// answer each gets: 0 with no error, which is "nothing to do" rather than a
// failure. A per-project system account already resolves a personal project id
// through the resolver's own second branch, so creating one would mean a
// project per project.
func TestEnsureSkipsIdentitiesThatMustNotOwnAPersonalProject(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)

	systemByEmail := seedUser(t, pool, "system_user_1@centry.user", "System One")
	systemByName := seedUser(t, pool, "svc@autotest.local", ":system:project:1:")
	suspended := seedUser(t, pool, "suspended@autotest.local", "Suspended")
	if _, err := pool.Exec(ctx,
		`UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}

	for name, userID := range map[string]int64{
		"system account by email":     systemByEmail,
		"system account by name":      systemByName,
		"suspended account":           suspended,
		"account that does not exist": 987_654,
	} {
		projectID, err := ensurer.Ensure(ctx, userID)
		if err != nil {
			t.Fatalf("%s: Ensure returned an error: %v", name, err)
		}
		if projectID != 0 {
			t.Fatalf("%s: Ensure created project %d", name, projectID)
		}
	}

	var created int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE name LIKE 'project_user_%'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("%d personal projects were created for identities that must have none", created)
	}
}

// A row carrying this user's personal-project name that the user is NOT a
// member of is not their personal project, however much the name looks like
// one — the resolver's first branch is membership-checked. Returning it
// reported success while /social/author went on answering "" for good.
//
// Reachable two ways, one cause: `POST /projects` takes the name as free text
// and reserves nothing, and pylon-era data carries `project_user_<uid>` rows
// whose role assignments are gone.
func TestEnsureIgnoresAProjectOfTheSameNameTheCallerIsNotAMemberOf(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	victim := seedUser(t, pool, "victim@autotest.local", "Victim")
	squatter := seedUser(t, pool, "squatter@autotest.local", "Squatter")

	var squattedID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO centry.project (name, owner_id, plugins, create_success)
		 VALUES ($1, $2, '{}', true) RETURNING id`,
		personalproject.Name(victim), squatter,
	).Scan(&squattedID); err != nil {
		t.Fatalf("seed the squatted project: %v", err)
	}

	provisioned, err := ensurer.Ensure(ctx, victim)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if provisioned == squattedID {
		t.Fatal("Ensure handed the caller a project they are not a member of")
	}

	// The squatted row is left alone: it is somebody else's project, and the
	// repair path must never delete one.
	var survivors int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, squattedID).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors != 1 {
		t.Fatalf("Ensure deleted project %d, which belongs to another user", squattedID)
	}

	// And the caller ends up with a project the resolver actually answers with.
	if resolved := resolveAuthorPersonalProjectID(ctx, t, pool, victim); resolved != provisioned {
		t.Fatalf("the author resolver answered %d, want the provisioned project %d", resolved, provisioned)
	}
}

// An id past int32 is refused, not TRUNCATED. `auth_core__user.id` is an
// `integer` and an advisory lock key is an int4, so a silent narrowing would
// take the lock of — and then read — a different account entirely:
// `int32(4294967305)` is 9.
func TestEnsureRefusesAUserIDItCannotNarrow(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	victim := seedUser(t, pool, "victim@autotest.local", "Victim")

	if _, err := ensurer.Ensure(ctx, int64(victim)+(1<<32)); err == nil {
		t.Fatal("an id past int32 was accepted")
	}

	var projects int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE name LIKE 'project_user_%'`).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 0 {
		t.Fatalf("%d personal project(s) were created for an out-of-range id", projects)
	}
}

// EnsureAsync is what the request handler calls, so it is what has to work:
// it must return before provisioning finishes, and the project must appear.
func TestEnsureAsyncProvisionsOffTheCallersGoroutine(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	userID := seedUser(t, pool, "async@autotest.local", "Async")

	ensurer.EnsureAsync(userID)
	// Called again immediately, exactly as the onboarding screen's five-second
	// poll would: the in-flight guard must collapse this into the first
	// attempt rather than start a second one.
	ensurer.EnsureAsync(userID)

	deadline := time.Now().Add(120 * time.Second)
	var resolved int64
	for time.Now().Before(deadline) {
		if resolved = resolveAuthorPersonalProjectID(ctx, t, pool, userID); resolved != 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if resolved == 0 {
		t.Fatal("EnsureAsync never produced a resolvable personal project")
	}

	var projects int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE name = $1`, personalproject.Name(userID),
	).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 1 {
		t.Fatalf("%d projects named %q, want 1", projects, personalproject.Name(userID))
	}
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// resolveAuthorPersonalProjectID runs the resolver's own decision tree — the
// query internal/api/v2/social/handler.go's resolvePersonalProjectID issues —
// so these tests measure the answer the SPA receives and not a restatement of
// it. Returns 0 when nothing resolves.
func resolveAuthorPersonalProjectID(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID int64,
) int64 {
	t.Helper()
	var projectID int64
	err := pool.QueryRow(ctx, `
		SELECT candidate.id
		FROM (
		    SELECT 1 AS priority, project.id AS id
		    FROM centry.project AS project
		    WHERE project.name = 'project_user_' || $1::integer::text
		      AND EXISTS (
		          SELECT 1
		          FROM public.auth_core__project_user_role AS assignment
		          WHERE assignment.project_id = project.id
		            AND assignment.user_id = $1::integer
		      )

		    UNION ALL

		    SELECT 2, substring(
		                  user_account.email
		                  FROM '^system_user_([0-9]+)@centry[.]user$'
		              )::integer
		    FROM public.auth_core__user AS user_account
		    WHERE user_account.id = $1::integer
		      AND user_account.email ~ '^system_user_[0-9]+@centry[.]user$'

		    UNION ALL

		    SELECT 3, assignment.project_id::integer
		    FROM public.auth_core__project_user_role AS assignment
		    WHERE assignment.user_id = $1::integer
		) AS candidate
		WHERE candidate.id IS NOT NULL
		ORDER BY candidate.priority, candidate.id
		LIMIT 1
	`, userID).Scan(&projectID)
	if err == pgx.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("resolve personal project for user %d: %v", userID, err)
	}
	return projectID
}

func seedUser(t *testing.T, pool *pgxpool.Pool, email, name string) int64 {
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

// newTestEnsurer builds the ensurer over the REAL provisioning pipeline, with
// the real secrets vault internal/api/router.go wires. A fake provisioner here
// would prove that Ensure calls something and nothing about whether what it
// created is a project the resolver can find — which is the entire defect.
func newTestEnsurer(t *testing.T, pool *pgxpool.Pool) *personalproject.Ensurer {
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

func newPersonalProjectPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := personalProjectDatabaseURL()
	if databaseURL == "" {
		t.Skipf("set %s to run the personal project integration test", databaseURLEnv)
	}
	if personalProjectTemplate == "" {
		t.Fatalf("TestMain did not build the personal project template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_personal_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, personalProjectTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURLEnv, err)
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

	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.CheckHead(ctx, migrate.ScopeShared, "platform"); err != nil {
		t.Fatalf("verify shared migration head: %v", err)
	}
	return pool
}
