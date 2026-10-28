package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// `credential-governance migrate-references` against the full ledgered
// migration corpus (dbtest template, copied per test). CI-only: skipped
// without ELITEA_TEST_DATABASE_URL. The template has no tenant toolkit or
// configuration table, so each test creates the p_7 tables it reads.

const (
	migrateTestDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
	migrateTestBootstrap      = "../../internal/infra/db/migrations/001_initial.sql"
	migrateTestSecretName     = "0123456789abcdef0123456789abcdef"
)

var (
	migrateTemplateOnce sync.Once
	migrateTemplate     string
	migrateTemplateErr  error
	migrateDBSeq        atomic.Uint64
)

// newMigrateTestDatabase returns a pool on a private migrated database and
// its DSN.
func newMigrateTestDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(migrateTestDatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the migrate-references integration tests", migrateTestDatabaseURLEnv)
	}
	ctx, cancel := dbtest.BuildContext(context.Background())
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	migrateTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(migrateTestBootstrap)
		if err != nil {
			migrateTemplateErr = err
			return
		}
		migrateTemplate, migrateTemplateErr = dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
			Files: platformmigrations.Files, Seed: string(bootstrap),
		})
	})
	if migrateTemplateErr != nil {
		t.Fatalf("build the migrate-references template: %v", migrateTemplateErr)
	}
	name := fmt.Sprintf("elitea_migrate_refs_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), migrateDBSeq.Add(1))
	if err := dbtest.CreateFromTemplate(ctx, adminPool, migrateTemplate, name); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", migrateTestDatabaseURLEnv, err)
	}
	parsed.Path = "/" + name
	dsn := parsed.String()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool, dsn
}

// seedMigrateWorld creates project 7 with the toolkits and configurations
// the rule covers, and some it skips or ignores.
func seedMigrateWorld(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO centry.project (id, name, owner_id) VALUES (7, 'migrate', 1) ON CONFLICT (id) DO NOTHING;
DROP SCHEMA IF EXISTS p_7 CASCADE;
CREATE SCHEMA p_7;
CREATE TABLE p_7.elitea_tools (
    id serial PRIMARY KEY, type varchar NOT NULL, name varchar(128),
    settings jsonb NOT NULL DEFAULT '{}'::jsonb, meta jsonb NOT NULL DEFAULT '{}'::jsonb);
CREATE TABLE p_7.configuration (
    id serial PRIMARY KEY, uuid uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(), project_id integer NOT NULL,
    elitea_title varchar NOT NULL UNIQUE, type varchar NOT NULL, section varchar NOT NULL DEFAULT 'credentials',
    data jsonb NOT NULL DEFAULT '{}'::jsonb, shared boolean NOT NULL DEFAULT false);
INSERT INTO p_7.configuration (uuid, project_id, elitea_title, type, data) VALUES
  ('11111111-1111-4111-8111-111111111111', 7, 'jira-prod', 'jira',
   '{"base_url": "https://Acme.Atlassian.net/", "username": "bot", "token": "{{secret.`+migrateTestSecretName+`}}"}'),
  ('22222222-2222-4222-8222-222222222222', 7, 'wiki', 'confluence', '{"base_url": "https://acme.atlassian.net/wiki"}'),
  ('33333333-3333-4333-8333-333333333333', 7, 'qtest-plain', 'qtest',
   '{"base_url": "https://qtest.example.com", "qtest_api_token": "plaintext-token"}'),
  ('44444444-4444-4444-8444-444444444444', 7, 'gh', 'github', '{"base_url": "https://api.github.com"}');
INSERT INTO p_7.elitea_tools (id, type, name, settings) VALUES
  (1, 'jira', 'jira a', '{"jira_configuration": {"elitea_title": "jira-prod", "private": false}}'),
  (2, 'jira', 'jira b', '{"jira_configuration": {"elitea_title": "jira-prod", "private": false}, "limit": 5}'),
  (3, 'confluence', 'wiki', '{"confluence_configuration": {"elitea_title": "wiki", "private": false}}'),
  (4, 'qtest', 'qtest', '{"qtest_configuration": {"elitea_title": "qtest-plain", "private": false}}'),
  (5, 'jira', 'mine', '{"jira_configuration": {"elitea_title": "jira-prod", "private": true}}'),
  (6, 'mcp', 'remote', '{"url": "https://MCP.example.com:443/mcp"}'),
  (7, 'mcp', 'headers', '{"url": "https://mcp.example.com/mcp", "headers": {"Authorization": "Bearer x"}}'),
  (8, 'github', 'gh', '{"github_configuration": {"elitea_title": "gh", "private": false}}'),
  (9, 'artifact', 'plain', '{"bucket": "b"}')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// snapshotTables fingerprints every base table the command can touch:
// row count and an md5 over every row, per table.
func snapshotTables(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
SELECT quote_ident(table_schema) || '.' || quote_ident(table_name) FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema IN ('elitea_identity', 'elitea_runtime', 'centry', 'p_7')
ORDER BY 1`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	out := make(map[string]string, len(tables))
	for _, table := range tables {
		var count int64
		var digest string
		if err := pool.QueryRow(ctx, `SELECT count(*), md5(COALESCE(string_agg(r::text, '|' ORDER BY r::text), '')) FROM `+table+` AS r`).
			Scan(&count, &digest); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		out[table] = fmt.Sprintf("%d:%s", count, digest)
	}
	return out
}

func runMigrateForTest(t *testing.T, dsn string, deps migrateDeps, args ...string) (int, []migrateItem, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	lookup := func(name string) (string, bool) {
		if name == "DATABASE_URL" {
			return dsn, true
		}
		return "", false
	}
	if deps.connect == nil {
		deps.connect = connectMigratePool
	}
	if deps.newID == nil {
		deps.newID = uuid.NewString
	}
	code := runMigrateReferences(context.Background(), args, lookup, &stdout, &stderr, deps)
	var items []migrateItem
	if slicesContains(args, "--json") {
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			if line == "" {
				continue
			}
			var item migrateItem
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				t.Fatalf("a --json line is not JSON: %q", line)
			}
			items = append(items, item)
		}
	}
	return code, items, stdout.String() + stderr.String()
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func byToolkit(items []migrateItem) map[int64]migrateItem {
	out := make(map[int64]migrateItem, len(items))
	for _, item := range items {
		out[item.ToolkitID] = item
	}
	return out
}

func countRefs(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_identity.credential_references WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The acceptance line of #1124: a dry run makes no writes.
func TestMigrateReferencesDryRunWritesNothing(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	seedMigrateWorld(t, pool)
	before := snapshotTables(t, pool)
	code, items, out := runMigrateForTest(t, dsn, migrateDeps{}, "--json")
	if code != 0 {
		t.Fatalf("dry run exit %d: %s", code, out)
	}
	if after := snapshotTables(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		for table, digest := range after {
			if before[table] != digest {
				t.Errorf("dry run changed %s: %s -> %s", table, before[table], digest)
			}
		}
		t.Fatal("the dry run wrote")
	}
	got := byToolkit(items)
	if len(items) != 8 {
		t.Fatalf("items = %d, want 8 (toolkit 9 holds no credential): %s", len(items), out)
	}
	jira := got[1]
	if jira.Action != "propose" || jira.Class != "static_secret" || jira.TargetIdentity != "https://acme.atlassian.net" ||
		jira.Backend != "pgvault" || jira.Locator != migrateTestSecretName || jira.Generation != 1 ||
		jira.SecretLocation != "pgvault:"+migrateTestSecretName+"@1" || jira.CredentialReferenceID != "" ||
		jira.ConfigurationUUID != "11111111-1111-4111-8111-111111111111" || jira.State != "staged" {
		t.Errorf("jira = %+v", jira)
	}
	if c := got[3]; c.Action != "propose" || c.Class != "oauth_delegated" || c.TargetIdentity != "https://acme.atlassian.net/wiki" {
		t.Errorf("confluence = %+v", c)
	}
	if m := got[6]; m.Action != "propose" || m.ProviderType != "mcp" || m.TargetIdentity != "https://mcp.example.com/mcp" {
		t.Errorf("mcp = %+v", m)
	}
	for toolkit, reason := range map[int64]string{4: "secret_not_sealed", 5: "private_credential_not_governed",
		7: "toolkit_not_governable", 8: "provider_not_governed"} {
		if got[toolkit].Action != "skipped" || got[toolkit].Reason != reason {
			t.Errorf("toolkit %d = %+v, want skipped %s", toolkit, got[toolkit], reason)
		}
	}
	// Human output says it wrote nothing.
	code, _, out = runMigrateForTest(t, dsn, migrateDeps{}, "--project", "7")
	if code != 0 || !strings.Contains(out, "Dry run: nothing was written") || !strings.Contains(out, "secret_not_sealed") {
		t.Fatalf("human dry run exit %d:\n%s", code, out)
	}
}

func TestMigrateReferencesApplyStagesOnlyAndIsIdempotent(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	seedMigrateWorld(t, pool)
	code, items, out := runMigrateForTest(t, dsn, migrateDeps{}, "--apply", "--operator", "100", "--project", "7", "--json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, out)
	}
	got := byToolkit(items)
	if got[1].Action != "staged" || got[2].Action != "exists" || got[1].CredentialReferenceID == "" ||
		got[1].CredentialReferenceID != got[2].CredentialReferenceID {
		t.Fatalf("toolkits sharing a configuration share one reference: %+v / %+v", got[1], got[2])
	}
	if got[3].Action != "staged" || got[6].Action != "staged" {
		t.Fatalf("confluence %+v, mcp %+v", got[3], got[6])
	}
	if n := countRefs(t, pool, "tenant_project_id = 7"); n != 3 {
		t.Fatalf("references = %d, want 3", n)
	}
	if n := countRefs(t, pool, "state = 'staged' AND staged_by = '100' AND activated_by IS NULL"); n != 3 {
		t.Fatalf("staged references by the operator = %d, want 3", n)
	}
	var settings string
	if err := pool.QueryRow(context.Background(), `SELECT settings::text FROM p_7.elitea_tools WHERE id = 1`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(settings, "credential_reference_id") {
		t.Fatalf("apply edited a toolkit: %s", settings)
	}
	var events int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event::text LIKE '%credential_reference.stage%'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 3 {
		t.Errorf("stage audit events = %d, want 3", events)
	}

	before := snapshotTables(t, pool)
	code, items, out = runMigrateForTest(t, dsn, migrateDeps{}, "--apply", "--operator", "100", "--project", "7", "--json")
	if code != 0 {
		t.Fatalf("second apply exit %d: %s", code, out)
	}
	for _, item := range items {
		if item.Action != "exists" && item.Action != "skipped" {
			t.Errorf("second apply %d = %s, want exists", item.ToolkitID, item.Action)
		}
	}
	if after := snapshotTables(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("a second apply wrote")
	}
}

func TestMigrateReferencesRefusesBadUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--apply"},
		{"--apply", "--operator", "alice"},
		{"--dry-run", "--apply", "--operator", "1"},
		{"--operator", "1"},
		{"--copy-to", "s3"},
		{"extra"},
	} {
		code, _, _ := runMigrateForTest(t, "postgres://unused", migrateDeps{
			connect: func(context.Context, string, bool) (*pgxpool.Pool, error) {
				t.Fatalf("%v connected", args)
				return nil, nil
			},
		}, args...)
		if code != 2 {
			t.Errorf("%v exit %d, want 2", args, code)
		}
	}
}

// fakeSecretStore is a single-mount, check-and-set KV store.
type fakeSecretStore struct {
	mu       sync.Mutex
	backend  string
	values   map[string][]string
	writes   int
	contexts []secretstore.StoreContext
}

func (s *fakeSecretStore) Read(_ context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, version int64) (secretstore.SecretVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := sc.Validate(secretstore.OpRead); err != nil {
		return secretstore.SecretVersion{}, err
	}
	s.contexts = append(s.contexts, sc)
	versions := s.values[ref.Path]
	if version <= 0 || int(version) > len(versions) || ref.Backend != s.backend {
		return secretstore.SecretVersion{}, secretstore.ErrNotFound
	}
	return secretstore.SecretVersion{Version: version, Value: secretstore.NewRuntimeSecret([]byte(versions[version-1]))}, nil
}

func (s *fakeSecretStore) Write(_ context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, value secretstore.RuntimeSecret, cas int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := sc.Validate(secretstore.OpWrite); err != nil {
		return 0, err
	}
	if int64(len(s.values[ref.Path])) != cas {
		return 0, secretstore.ErrVersionConflict
	}
	s.writes++
	s.values[ref.Path] = append(s.values[ref.Path], string(value.UnsafeBytes()))
	return cas + 1, nil
}

func (s *fakeSecretStore) Metadata(context.Context, secretstore.StoreContext, secretstore.SecretRef) (secretstore.Metadata, error) {
	return secretstore.Metadata{}, nil
}

func (s *fakeSecretStore) Retire(context.Context, secretstore.StoreContext, secretstore.SecretRef, int64) error {
	return nil
}

func TestMigrateReferencesCopiesToVaultThroughTheLedger(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	seedMigrateWorld(t, pool)
	source := &fakeSecretStore{backend: secretstore.BackendPGVault, values: map[string][]string{migrateTestSecretName: {"jira-token"}}}
	vault := &fakeSecretStore{backend: secretstore.BackendVault, values: map[string][]string{}}
	deps := migrateDeps{secrets: func(func(string) (string, bool), *pgxpool.Pool) (migrateSecrets, error) {
		return migrateSecrets{source: source, vault: vault, mount: "kv"}, nil
	}}
	args := []string{"--apply", "--operator", "100", "--project", "7", "--copy-to", "vault", "--policy-revision", "2026-10-r1", "--json"}
	code, items, out := runMigrateForTest(t, dsn, deps, args...)
	if code != 0 {
		t.Fatalf("copy exit %d: %s", code, out)
	}
	jira := byToolkit(items)[1]
	path := "elitea/projects/7/credentials/11111111-1111-4111-8111-111111111111"
	if jira.Action != "staged" || jira.Backend != "vault" || jira.Locator != path || jira.Generation != 1 {
		t.Fatalf("jira = %+v", jira)
	}
	if got := vault.values[path]; len(got) != 1 || got[0] != "jira-token" {
		t.Fatalf("vault holds %v", got)
	}
	if len(source.contexts) != 1 || source.contexts[0].WorkloadScope != migrateReferencesCopyScope ||
		source.contexts[0].CredentialRefID != jira.CredentialReferenceID || source.contexts[0].DecisionID == "" {
		t.Fatalf("source read context = %+v", source.contexts)
	}
	var committed int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_identity.secret_write_ledger
WHERE state = 'committed' AND backend = 'vault' AND locator_path = $1`, path).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 1 {
		t.Fatalf("committed ledger rows = %d, want 1", committed)
	}
	if n := countRefs(t, pool, "backend = 'pgvault' AND class = 'static_secret'"); n != 0 {
		t.Fatalf("a pgvault static reference was staged too: %d", n)
	}
	// A rerun neither writes nor stages again.
	code, items, out = runMigrateForTest(t, dsn, deps, args...)
	if code != 0 || byToolkit(items)[1].Action != "exists" || vault.writes != 1 {
		t.Fatalf("rerun exit %d, jira %+v, writes %d: %s", code, byToolkit(items)[1], vault.writes, out)
	}
	// A crash between the Vault write and the staging: the path already
	// holds the same value, so it is reused at version 1, never overwritten.
	if _, err := pool.Exec(context.Background(), `DELETE FROM elitea_identity.credential_references WHERE backend = 'vault'`); err != nil {
		t.Fatal(err)
	}
	code, items, out = runMigrateForTest(t, dsn, deps, args...)
	if code != 0 || byToolkit(items)[1].Action != "staged" || byToolkit(items)[1].Generation != 1 || vault.writes != 1 {
		t.Fatalf("recovery exit %d, jira %+v, writes %d: %s", code, byToolkit(items)[1], vault.writes, out)
	}
	// A different value at the path stops the run and overwrites nothing.
	vault.values[path] = []string{"someone-else"}
	if _, err := pool.Exec(context.Background(), `DELETE FROM elitea_identity.credential_references WHERE backend = 'vault'`); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = runMigrateForTest(t, dsn, deps, args...); code != 1 || vault.values[path][0] != "someone-else" {
		t.Fatalf("a foreign value: exit %d, vault %v", code, vault.values[path])
	}
}
