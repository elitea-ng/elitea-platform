package pgvault

// The adapter against a real PostgreSQL and the real vault reader and writer:
// it returns byte-identical values to the direct storage.SecretVaultLoader
// path, its writes are visible to that path, and two concurrent creates of one
// name cannot both succeed.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

func newVaultPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf("elitea_pgvault_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})
	// The two vault tables, in the shape the deployed schema has.
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.secrets_key (id TEXT PRIMARY KEY, data BYTEA);
CREATE TABLE centry.secrets_data (id TEXT PRIMARY KEY, data BYTEA);`); err != nil {
		t.Fatal(err)
	}
	return pool
}

type vaultFixture struct {
	pool   *pgxpool.Pool
	loader *storage.PostgresSecretVaultLoader
	writer *repos.CurrentSecretVaultRepository
	store  *Store
}

func newVaultFixture(t *testing.T) vaultFixture {
	t.Helper()
	pool := newVaultPool(t)
	handler := v2secrets.NewHandler(pool)
	writer, err := repos.NewCurrentSecretVaultRepository(pool, nil, repos.WithProjectVaultCreator(handler))
	if err != nil {
		t.Fatal(err)
	}
	loader, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loader.Destroy)
	store, err := New(pool, loader, writer)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.EnsureProjectVault(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	return vaultFixture{pool: pool, loader: loader, writer: writer, store: store}
}

func (f vaultFixture) direct(t *testing.T, project int64, name string) (centrysecrets.Secret, error) {
	t.Helper()
	vault, err := f.loader.LoadProjectVault(context.Background(), project)
	if err != nil {
		return centrysecrets.Secret{}, err
	}
	return vault.Lookup(name)
}

func TestPostgresPGVaultReadsAreByteIdenticalToTheDirectPath(t *testing.T) {
	f := newVaultFixture(t)
	ctx := context.Background()
	values := map[string]string{
		"legacy_regular": "regular-value-é-✓",
		"legacy_hidden":  "  spaced\nmultiline\tvalue  ",
	}
	if err := f.writer.MutateProject(ctx, 7, []centrysecrets.Mutation{
		{Collection: centrysecrets.RegularSecrets, Name: "legacy_regular", Value: values["legacy_regular"]},
		{Collection: centrysecrets.HiddenSecrets, Name: "legacy_hidden", Value: values["legacy_hidden"]},
	}); err != nil {
		t.Fatal(err)
	}
	for name := range values {
		direct, err := f.direct(t, 7, name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.store.Read(ctx, sc(), ref(name), 1)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Value.UnsafeBytes()) != direct.Value || direct.Value != values[name] {
			t.Fatalf("%s: adapter and direct path disagree", name)
		}
	}
}

func TestPostgresPGVaultWriteRetireLifecycle(t *testing.T) {
	f := newVaultFixture(t)
	ctx := context.Background()
	const value = "governed-value-42"
	version, err := f.store.Write(ctx, sc(), ref("governed_secret"), secretstore.NewRuntimeSecret([]byte(value)), 0)
	if err != nil || version != 1 {
		t.Fatalf("write = %d, %v", version, err)
	}
	direct, err := f.direct(t, 7, "governed_secret")
	if err != nil || direct.Value != value || !direct.Hidden {
		t.Fatalf("direct after write = %+v, %v", direct, err)
	}
	if _, err := f.store.Write(ctx, sc(), ref("governed_secret"), secretstore.NewRuntimeSecret([]byte("other")), 0); !errors.Is(err, secretstore.ErrVersionConflict) {
		t.Fatalf("second create = %v", err)
	}
	if err := f.store.Retire(ctx, sc(), ref("governed_secret"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.direct(t, 7, "governed_secret"); !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		t.Fatalf("direct after retire = %v", err)
	}
	if _, err := f.store.Read(ctx, sc(), ref("governed_secret"), 1); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("read after retire = %v", err)
	}
	// A retire of an absent secret succeeds.
	if err := f.store.Retire(ctx, sc(), ref("governed_secret"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresPGVaultConcurrentCreatesHaveOneWinner(t *testing.T) {
	f := newVaultFixture(t)
	var wins, conflicts atomic.Int64
	var wg sync.WaitGroup
	for index := range 8 {
		wg.Go(func() {
			value := secretstore.NewRuntimeSecret(fmt.Appendf(nil, "value-%d", index))
			_, err := f.store.Write(context.Background(), sc(), ref("raced_secret"), value, 0)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, secretstore.ErrVersionConflict):
				conflicts.Add(1)
			default:
				t.Errorf("write = %v", err)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 7 {
		t.Fatalf("wins=%d conflicts=%d", wins.Load(), conflicts.Load())
	}
}

func TestPostgresPGVaultCreatesAnAbsentProjectVault(t *testing.T) {
	f := newVaultFixture(t)
	context8 := sc()
	context8.TenantProjectID = 8
	if _, err := f.store.Read(context.Background(), context8, ref("x"), 1); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("read without a vault = %v", err)
	}
	if _, err := f.store.Write(context.Background(), context8, ref("first_secret"), secretstore.NewRuntimeSecret([]byte("v")), 0); err != nil {
		t.Fatal(err)
	}
	if direct, err := f.direct(t, 8, "first_secret"); err != nil || direct.Value != "v" {
		t.Fatalf("direct = %+v, %v", direct, err)
	}
}
