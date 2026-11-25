package migrate_test

// Shared 0265 (issue 2505, ADR-0059 R1.1): auth_core__token.admin_access and
// admin_scopes, the `inherit` backfill and the `none` default.
//
// Each test builds a database from 001_initial.sql and the shared corpus
// WITHOUT 0265 (and any file after it), writes the credential rows through the
// production code of the release before (personal access tokens through the
// sqlc query, native device sessions through the native authorization store),
// and then upgrades with the production runner. Runs when
// ELITEA_TEST_DATABASE_URL is set; skips otherwise.
//
// The resolver does not read the columns until R1.2a (issue 2506). So
// "resolves the same as before" is checked against today's resolver here;
// once R1.2a merges, the same test is the proof that `inherit` keeps the
// owner's set.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	tokenAdminAccessFile      = "0265_token_admin_access.sql"
	tokenAdminAccessMigration = "shared/" + tokenAdminAccessFile
	tokenAdminAccessDBEnv     = "ELITEA_TEST_DATABASE_URL"
)

var tokenAdminAccessDBSeq atomic.Uint64

// beforeFile hides one shared migration and every shared file after it, so
// the corpus it serves stops just before that file.
type beforeFile struct {
	fs.FS
	first string
}

func (b beforeFile) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(b.FS, name)
	if err != nil {
		return nil, err
	}
	if name != "shared" {
		return entries, nil
	}
	kept := entries[:0:0]
	for _, entry := range entries {
		if entry.Name() < b.first {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

func newTokenAdminAccessDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(tokenAdminAccessDBEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the token administration access upgrade tests", tokenAdminAccessDBEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_tokenadmin_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), tokenAdminAccessDBSeq.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", tokenAdminAccessDBEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// newDatabaseBefore0265 is 001_initial.sql plus the shared corpus before 0265.
func newDatabaseBefore0265(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newTokenAdminAccessDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := pool.Exec(ctx, bootstrapschema.Initial); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	corpus := beforeFile{FS: platformmigrations.Files, first: tokenAdminAccessFile}
	if err := migrate.New(pool, corpus).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus before %s: %v", tokenAdminAccessFile, err)
	}
	if hasColumn(t, pool, "admin_access") {
		t.Fatal("premise: the corpus before 0265 already has auth_core__token.admin_access")
	}
	return pool
}

// upgrade applies the rest of the shared corpus, 0265 included, with the
// production runner.
func upgrade(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("upgrade the shared corpus: %v", err)
	}
	if !hasColumn(t, pool, "admin_access") || !hasColumn(t, pool, "admin_scopes") {
		t.Fatal("the upgrade did not add admin_access and admin_scopes")
	}
}

func hasColumn(t *testing.T, pool *pgxpool.Pool, column string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public' AND table_name = 'auth_core__token'
		                 AND column_name = $1)`, column).Scan(&present); err != nil {
		t.Fatalf("read the auth_core__token columns: %v", err)
	}
	return present
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}

// newUser inserts an account and assigns it the named administration roles.
func newUser(t *testing.T, pool *pgxpool.Pool, email string, adminRoles ...string) int32 {
	t.Helper()
	var id int32
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1) RETURNING id`, email,
	).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	for _, role := range adminRoles {
		assignAdminRole(t, pool, id, role)
	}
	return id
}

func assignAdminRole(t *testing.T, pool *pgxpool.Pool, userID int32, role string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `
		INSERT INTO public.auth_core__user_role (user_id, role_id)
		SELECT $1, role.id FROM public.auth_core__role AS role
		WHERE role.name = $2 AND role.mode = 'administration'
		ON CONFLICT (user_id, role_id) DO NOTHING`, userID, role)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("assign the administration role %s to %d: %v (rows %d)", role, userID, err, tag.RowsAffected())
	}
}

// createPAT is the personal access token insert site (auth_pat.sql,
// CreatePATForActiveUser), as the token handler and the actor PAT call it.
func createPAT(t *testing.T, pool *pgxpool.Pool, userID int32, name string) sqlcgen.CreatePATForActiveUserRow {
	t.Helper()
	row, err := sqlcgen.New(pool).CreatePATForActiveUser(context.Background(), sqlcgen.CreatePATForActiveUserParams{
		Uuid:   uuid.NewString(),
		UserID: userID,
		Name:   &name,
	})
	if err != nil {
		t.Fatalf("create a personal access token for %d: %v", userID, err)
	}
	return row
}

// createPreviousReleasePAT is the personal access token insert of the
// release before 0265: CreatePATForActiveUser as it was, which names no
// administration column and returns none (the current query returns them,
// so it cannot run before the upgrade).
func createPreviousReleasePAT(t *testing.T, pool *pgxpool.Pool, userID int32, name string) int32 {
	t.Helper()
	var id int32
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
		SELECT $1::varchar(36), NULL, owner.id, $3::text
		FROM public.auth_core__user AS owner
		WHERE owner.id = $2::integer AND owner.suspended = false
		RETURNING id`, uuid.NewString(), userID, name).Scan(&id); err != nil {
		t.Fatalf("create a personal access token for %d: %v", userID, err)
	}
	return id
}

// createNativeSession runs the native authorization code flow (ADR-0025) and
// returns the id of the device anchor row it wrote on auth_core__token.
func createNativeSession(t *testing.T, pool *pgxpool.Pool, userID int32) int32 {
	t.Helper()
	ctx := context.Background()
	store := nativeauth.NewStore(pool, nativeauth.Config{})
	const (
		clientID    = "token-admin-access-test"
		redirectURI = "elitea-test://callback"
		verifier    = "token-admin-access-test-verifier-0123456789-abcdefghijklmnop"
	)
	sum := sha256.Sum256([]byte(verifier))
	handle, _, err := store.CreateAuthorization(ctx, nativeauth.AuthorizationRequest{
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		State:         "state",
		DeviceName:    "test device",
		Platform:      "ios",
		ClientVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("create a native authorization: %v", err)
	}
	authorization, err := store.AuthorizationByHandle(ctx, handle)
	if err != nil {
		t.Fatalf("read the native authorization: %v", err)
	}
	decision, err := store.Decide(ctx, authorization.ID, true, int64(userID))
	if err != nil {
		t.Fatalf("approve the native authorization: %v", err)
	}
	grant, _, err := store.ExchangeCode(ctx, nativeauth.ExchangeRequest{
		Code:              decision.Code,
		ClientID:          clientID,
		RedirectURI:       redirectURI,
		Verifier:          verifier,
		FileClientEnabled: true,
	})
	if err != nil {
		t.Fatalf("exchange the native code: %v", err)
	}
	var anchor *int32
	if err := pool.QueryRow(ctx,
		`SELECT token_id FROM elitea_auth.native_sessions WHERE id::text = $1`, grant.DeviceID,
	).Scan(&anchor); err != nil || anchor == nil {
		t.Fatalf("read the native device anchor of %s: %v", grant.DeviceID, err)
	}
	return *anchor
}

type adminAccess struct {
	access string
	scopes []string
}

func readAdminAccess(t *testing.T, pool *pgxpool.Pool, tokenID int32) adminAccess {
	t.Helper()
	var got adminAccess
	if err := pool.QueryRow(context.Background(),
		`SELECT admin_access, admin_scopes FROM public.auth_core__token WHERE id = $1`, tokenID,
	).Scan(&got.access, &got.scopes); err != nil {
		t.Fatalf("read the administration access of token %d: %v", tokenID, err)
	}
	return got
}

func assertNone(t *testing.T, pool *pgxpool.Pool, what string, tokenID int32) {
	t.Helper()
	got := readAdminAccess(t, pool, tokenID)
	if got.access != "none" || len(got.scopes) != 0 {
		t.Errorf("%s (token %d): admin_access %q, admin_scopes %v; want none and empty", what, tokenID, got.access, got.scopes)
	}
}

// resolveAdministration is the resolver over a personal access token
// principal (credential kind `token`, ADR-0059 R1.2a). It reads admin_access,
// so it runs only after 0265.
func resolveAdministration(t *testing.T, pool *pgxpool.Pool, tokenID, userID int32) []string {
	t.Helper()
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(context.Background(), auth.User{
		TokenID:        strconv.Itoa(int(tokenID)),
		UserID:         strconv.Itoa(int(userID)),
		AuthType:       "token",
		CredentialKind: auth.CredentialKindToken,
	}, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatalf("resolve the administration set of token %d: %v", tokenID, err)
	}
	return resolution.Permissions
}

// resolvePreviousRelease is what the previous release resolved for a token:
// its resolver ignored the credential, so a token resolved its owner's whole
// administration set, the set a browser session of the owner resolves now.
func resolvePreviousRelease(t *testing.T, pool *pgxpool.Pool, userID int32) []string {
	t.Helper()
	id := strconv.Itoa(int(userID))
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(context.Background(), auth.User{
		ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession,
	}, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatalf("resolve the administration set of user %d: %v", userID, err)
	}
	return resolution.Permissions
}

/* ── the backfill ─────────────────────────────────────────────────────── */

// After the upgrade every existing personal access token resolves the same
// administration set as before, over an owner with and an owner without an
// administration role; only the first is `inherit`.
func TestTokenAdminAccessBackfillKeepsWhatEveryTokenResolves(t *testing.T) {
	pool := newDatabaseBefore0265(t)

	administrator := newUser(t, pool, "administrator@example.test", "admin")
	plain := newUser(t, pool, "plain@example.test")
	// A role without permissions does not count.
	mustExec(t, pool, `INSERT INTO public.auth_core__role (name, mode) VALUES ('no_grants', 'administration')`)
	emptyRole := newUser(t, pool, "empty-role@example.test", "no_grants")

	type token struct {
		what  string
		id    int32
		owner int32
		want  string
	}
	tokens := []token{
		{"the administrator's token", createPreviousReleasePAT(t, pool, administrator, "ci"), administrator, "inherit"},
		{"the administrator's second token", createPreviousReleasePAT(t, pool, administrator, "backup"), administrator, "inherit"},
		{"the plain user's token", createPreviousReleasePAT(t, pool, plain, "ci"), plain, "none"},
		{"the token of a role without permissions", createPreviousReleasePAT(t, pool, emptyRole, "ci"), emptyRole, "none"},
	}
	before := map[int32][]string{}
	for _, tok := range tokens {
		before[tok.id] = resolvePreviousRelease(t, pool, tok.owner)
	}
	if len(before[tokens[0].id]) == 0 {
		t.Fatal("premise: the administrator's token resolves no administration permission before the upgrade")
	}
	if len(before[tokens[2].id]) != 0 || len(before[tokens[3].id]) != 0 {
		t.Fatal("premise: a token of an owner without administration permissions resolves some before the upgrade")
	}

	upgrade(t, pool)

	for _, tok := range tokens {
		got := readAdminAccess(t, pool, tok.id)
		if got.access != tok.want || len(got.scopes) != 0 {
			t.Errorf("%s: admin_access %q, admin_scopes %v; want %q and empty", tok.what, got.access, got.scopes, tok.want)
		}
		// The token resolves what its owner's session resolves after the
		// upgrade, and loses nothing it resolved before. A later migration of
		// the corpus may grant the owner's role a new permission (0273 gives
		// admin entitlements.export.manage), which an `inherit` token then
		// resolves too, so the sets before and after are not compared for
		// equality.
		after := resolveAdministration(t, pool, tok.id, tok.owner)
		if owner := resolvePreviousRelease(t, pool, tok.owner); !slices.Equal(after, owner) {
			t.Errorf("%s resolves %v after the upgrade; its owner's session resolves %v", tok.what, after, owner)
		}
		for _, name := range before[tok.id] {
			if !slices.Contains(after, name) {
				t.Errorf("%s resolved %s before the upgrade and not after", tok.what, name)
			}
		}
	}

	// A token whose owner held no administration permission at migration
	// time stays `none` when the owner is given `admin` later, and resolves
	// nothing (R1.2a).
	assignAdminRole(t, pool, plain, "admin")
	assertNone(t, pool, "the plain user's token after the owner became admin", tokens[2].id)
	if got := resolveAdministration(t, pool, tokens[2].id, plain); len(got) != 0 {
		t.Errorf("the plain user's token resolves %v after the owner became admin, want nothing", got)
	}
}

// A native device session and the project system token are not personal
// access tokens: they stay `none` even when their owner holds an
// administration permission.
func TestTokenAdminAccessBackfillLeavesNativeSessionsAndSystemTokensNone(t *testing.T) {
	pool := newDatabaseBefore0265(t)

	administrator := newUser(t, pool, "device-owner@example.test", "admin")
	anchor := createNativeSession(t, pool, administrator)

	// The system identity of the seeded project 1, given an administration
	// role so that only the exclusion can keep its token at `none`.
	systemUser := newUser(t, pool, "system_user_1@centry.user", "admin")
	systemToken := createPreviousReleasePAT(t, pool, systemUser, "api")

	upgrade(t, pool)

	assertNone(t, pool, "the native device anchor", anchor)
	assertNone(t, pool, "the project system token", systemToken)
}

/* ── the default ──────────────────────────────────────────────────────── */

// A token created after the upgrade, through each of the three insert sites,
// has `none` and no scopes, whoever its owner is.
func TestTokenAdminAccessNewRowsDefaultToNone(t *testing.T) {
	pool := newDatabaseBefore0265(t)
	upgrade(t, pool)
	ctx := context.Background()

	administrator := newUser(t, pool, "new-administrator@example.test", "admin")

	pat := createPAT(t, pool, administrator, "after-upgrade")
	if pat.AdminAccess != "none" || len(pat.AdminScopes) != 0 {
		t.Errorf("CreatePATForActiveUser returned admin_access %q, admin_scopes %v; want none and empty", pat.AdminAccess, pat.AdminScopes)
	}
	assertNone(t, pool, "a personal access token created after the upgrade", pat.ID)

	assertNone(t, pool, "a native device anchor created after the upgrade", createNativeSession(t, pool, administrator))

	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
	provisionCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := provisioner.Provision(provisionCtx, projectprovisioning.Request{
		Name:    "Token admin access",
		OwnerID: 1,
		Plugins: []string{"configuration", "models"},
		Limits:  projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision a project: %v (steps %+v)", err, result.Steps)
	}
	system, err := sqlcgen.New(pool).GetActiveProjectSystemPAT(ctx, int32(result.ProjectID))
	if err != nil {
		t.Fatalf("read the system token of project %d: %v", result.ProjectID, err)
	}
	assertNone(t, pool, "the project system token created after the upgrade", system.TokenID)
}

/* ── re-run and guard ─────────────────────────────────────────────────── */

// Applying the file again backfills nothing: a token created after the
// upgrade whose owner holds `admin` stays `none`.
func TestTokenAdminAccessRerunDoesNotBackfill(t *testing.T) {
	pool := newDatabaseBefore0265(t)
	upgrade(t, pool)

	administrator := newUser(t, pool, "rerun@example.test", "admin")
	tokenID := createPAT(t, pool, administrator, "after-upgrade").ID

	notices := applyFileCollectingNotices(t, pool)
	if !containsNotice(notices, "backfill skipped") {
		t.Errorf("the second run raised %q, want the notice that the backfill was skipped", notices)
	}
	assertNone(t, pool, "a token created after the upgrade, after a second run", tokenID)
}

// On a database without public.auth_core__token the file changes nothing and
// says so.
func TestTokenAdminAccessIsANoOpWithoutTheTokenTable(t *testing.T) {
	pool := newTokenAdminAccessDatabase(t)
	countRelations := func() int {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_class AS c
			JOIN pg_namespace AS n ON n.oid = c.relnamespace
			WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')`).Scan(&n); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return n
	}
	before := countRelations()
	for run := range 2 {
		notices := applyFileCollectingNotices(t, pool)
		if !containsNotice(notices, "public.auth_core__token absent") {
			t.Errorf("run %d raised %q, want the notice that public.auth_core__token is absent", run+1, notices)
		}
	}
	if after := countRelations(); after != before {
		t.Errorf("0265 created %d relations on a database without auth_core__token", after-before)
	}
}

func containsNotice(notices []string, fragment string) bool {
	for _, notice := range notices {
		if strings.Contains(notice, "0265") && strings.Contains(notice, fragment) {
			return true
		}
	}
	return false
}

// applyFileCollectingNotices runs 0265 on its own connection, in one
// transaction as the runner does, and returns the notices it raised.
func applyFileCollectingNotices(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	body, err := fs.ReadFile(platformmigrations.Files, tokenAdminAccessMigration)
	if err != nil {
		t.Fatalf("read %s: %v", tokenAdminAccessMigration, err)
	}
	var (
		mu      sync.Mutex
		notices []string
	)
	config := pool.Config().ConnConfig.Copy()
	config.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, notice.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(body))
		return err
	}); err != nil {
		t.Fatalf("apply %s: %v", tokenAdminAccessMigration, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(notices)
}
