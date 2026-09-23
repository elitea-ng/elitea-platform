package artifacts_test

// The SDK's by-filepath object read (#978) over a REAL migrated database.
//
// sdk_artifact_path_test.go runs the same route against the in-memory double,
// so it proves the DECISIONS. It cannot prove that the rows those decisions
// read exist in the shape the handler expects: the bucket lookup and the
// per-bucket access list both come out of `elitea_storage` (shared migrations
// 0057 and 0118), and a double that answers from a Go map agrees with
// whatever the handler asks for. That is exactly how an alias route can be
// green in unit tests and 404 or 403 on a live stack — which is the class of
// defect this issue is, the route having been absent altogether.
//
// Same template-database technique
// internal/api/webhook/dispatcher_postgres_integration_test.go uses: one
// ledgered-migration template built in TestMain, copied per test. No tenant
// schema is needed — every artifact table is shared-schema with a plain
// `project_id` column — so Spec.Tenants stays empty.
//
// Skips (not fails) with no ELITEA_TEST_DATABASE_URL, like every other
// _postgres_integration_test.go in this service.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	sdkArtifactDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
	sdkArtifactBootstrap      = "../../../infra/db/migrations/001_initial.sql"
)

var sdkArtifactTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(sdkArtifactDatabaseURLEnv)
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(sdkArtifactBootstrap)
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
		Files: platformmigrations.Files,
		Seed:  string(bootstrap),
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build the artifacts template: %v\n", err)
		os.Exit(1)
	}
	sdkArtifactTemplate = templateName
	os.Exit(m.Run())
}

func newSDKArtifactPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(sdkArtifactDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", sdkArtifactDatabaseURLEnv)
	}
	if sdkArtifactTemplate == "" {
		t.Fatalf("TestMain did not build the artifacts template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_sdk_artifact_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, sdkArtifactTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", sdkArtifactDatabaseURLEnv, err)
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

// postgresArtifactRepo is the same union internal/api/router.go's
// artifactRepoAdapter builds, assembled here so the handler under test holds
// the REAL repositories rather than the in-memory double.
type postgresArtifactRepo struct {
	*repos.ArtifactBucketsRepository
	*repos.ArtifactObjectsRepository
	*repos.ArtifactTransferGrantsRepository
	*repos.ArtifactBucketPermissionsRepository
}

func newPostgresArtifactHandler(t *testing.T, pool *pgxpool.Pool) (*artifacts.Handler, *fakeStore) {
	t.Helper()
	buckets, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("buckets repository: %v", err)
	}
	objects, err := repos.NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("objects repository: %v", err)
	}
	grants, err := repos.NewArtifactTransferGrantsRepository(pool)
	if err != nil {
		t.Fatalf("grants repository: %v", err)
	}
	permissions, err := repos.NewArtifactBucketPermissionsRepository(pool)
	if err != nil {
		t.Fatalf("bucket permissions repository: %v", err)
	}
	// The object BYTES are the one thing no artifact table holds: the store is
	// S3/Azure/GCS in production and there is no local-disk backend to stand
	// in for it here. The double keeps the bytes; everything the route decides
	// — the bucket, the access list — comes out of PostgreSQL.
	store := newFakeStore()
	return artifacts.NewHandler(postgresArtifactRepo{buckets, objects, grants, permissions}, store), store
}

func seedPostgresBucket(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) {
	t.Helper()
	buckets, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("buckets repository: %v", err)
	}
	if _, err := buckets.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: projectID, Name: name, DisplayName: name, BucketType: "local",
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
}

func sdkArtifactRouter(h *artifacts.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/artifact/default/{projectID}/{bucket}/*", h.DownloadObject)
	return r
}

// The route the SDK calls answers the object's bytes for a bucket that exists
// in `elitea_storage.buckets`, and the typed 404 for one that does not.
func TestSDKArtifactPathReadsAPostgresBackedBucket(t *testing.T) {
	pool := newSDKArtifactPool(t)
	seedPostgresBucket(t, pool, 1, "reports")
	handler, store := newPostgresArtifactHandler(t, pool)
	store.seedContent("1", "reports", "run-12/summary.txt", []byte("attach me"), "")

	rr := httptest.NewRecorder()
	sdkArtifactRouter(handler).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/run-12/summary.txt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "attach me" {
		t.Fatalf("body = %q, want the raw object bytes", rr.Body.String())
	}

	absent := httptest.NewRecorder()
	sdkArtifactRouter(handler).ServeHTTP(absent,
		httptest.NewRequest(http.MethodGet, "/artifact/default/1/no-such-bucket/summary.txt", nil))
	if absent.Code != http.StatusNotFound {
		t.Fatalf("absent bucket status = %d, want 404; body=%s", absent.Code, absent.Body.String())
	}
}

// The per-bucket access list stored by shared/0118 refuses the alias for a
// blocked member, and leaves an unlisted member alone.
func TestSDKArtifactPathHonoursTheStoredBucketAccessList(t *testing.T) {
	pool := newSDKArtifactPool(t)
	seedPostgresBucket(t, pool, 1, "reports")
	handler, store := newPostgresArtifactHandler(t, pool)
	store.seedContent("1", "reports", "a.txt", []byte("secret"), "")

	permissions, err := repos.NewArtifactBucketPermissionsRepository(pool)
	if err != nil {
		t.Fatalf("bucket permissions repository: %v", err)
	}
	// An EMPTY list is "no access" — the row exists and grants nothing, which
	// is a different decision from having no row at all.
	if err := permissions.ReplaceUserBucketPermissions(t.Context(), 1, 7,
		map[string][]string{"reports": {}}); err != nil {
		t.Fatalf("store the exception: %v", err)
	}

	blocked := httptest.NewRecorder()
	sdkArtifactRouter(handler).ServeHTTP(blocked,
		withMember(httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/a.txt", nil), 7))
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("blocked member status = %d, want 403; body=%s", blocked.Code, blocked.Body.String())
	}

	allowed := httptest.NewRecorder()
	sdkArtifactRouter(handler).ServeHTTP(allowed,
		withMember(httptest.NewRequest(http.MethodGet, "/artifact/default/1/reports/a.txt", nil), 8))
	if allowed.Code != http.StatusOK {
		t.Fatalf("unlisted member status = %d, want 200; body=%s", allowed.Code, allowed.Body.String())
	}
}
