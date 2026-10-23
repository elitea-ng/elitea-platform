package contract

// S19: self-contained conformance suite driving the new /api/v2/artifacts/*
// REST API end to end — real router (internal/api.NewRouter), real
// Postgres (ephemeral CREATE DATABASE + the same embedded shared migrations
// every other Postgres-integration package in this repo applies), and a
// real S3-compatible backend (RustFS, the same emulator
// internal/infra/storage/conformance drives — see
// deploy/docker-compose.storage-test.yml).
//
// Built once for the whole test binary (not per-test): every TestArtifact*
// test shares one Postgres database and one physical RustFS bucket, each
// test using its own project ID (artifactNextProjectID) and bucket name to
// avoid cross-test interference, matching the rest of this repo's
// established "unique-per-test disambiguator instead of isolated
// infrastructure" convention for cheap, fast Postgres-integration suites.

import (
	"context"
	"errors"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	platformauth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/s3"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	artifactTestDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
	artifactContractBucket     = "elitea-contract-test"
)

// artifactServer is the shared harness every TestArtifact* test uses,
// or nil when ELITEA_TEST_DATABASE_URL / S3_ENDPOINT_URL are unset — built
// once in TestMain, torn down once after m.Run() returns.
var artifactServer *httptest.Server

// TestMain builds the shared artifact harness once for the whole binary and
// tears it down after m.Run() returns. TestMain, not t.Cleanup, is the right
// place for setup shared across many independent top-level test functions.
// It never exits early: absent env vars make each TestArtifact* test skip
// itself (requireArtifactSuite), so the package's other tests still run.
func TestMain(m *testing.M) {
	teardown := setupArtifactSuite()
	code := m.Run()
	teardown()
	os.Exit(code)
}

// artifactPool is the same pool the router's real handlers use, exposed for
// tests that need to seed a row (elitea_storage.project_storage_policy) or
// manipulate one directly (forcing a grant's expires_at into the past) that
// has no dedicated API endpoint — real Postgres access, not a second
// database, so this always reflects exactly what the router itself sees.
var artifactPool *pgxpool.Pool

// artifactProjectIDCounter hands out a fresh project ID per test —
// TestArtifact* functions run sequentially (none call t.Parallel), but a
// package-level atomic counter costs nothing and removes any doubt. Seeded
// from wall-clock time, not a fixed literal: the ephemeral Postgres database
// is fresh every run, but the physical RustFS bucket is not recreated (only
// idempotently ensured) — a fixed starting value would let a leftover
// object from a previous local `go test` invocation collide with a "first
// upload" assertion in a later run against the same long-lived emulator
// container. CI gets a fresh RustFS container every run regardless, but
// this is what makes repeated local iteration against one running stack
// safe too. Millisecond epoch time, not UnixNano: a project ID must match
// ^[1-9][0-9]{0,17}$ (storage.NewBucketRef, at most 18 digits) — UnixNano()
// is already 19 digits as of 2026 and would fail that check.
var artifactProjectIDCounter = time.Now().UnixMilli()

func nextArtifactProjectID() int64 {
	return atomic.AddInt64(&artifactProjectIDCounter, 1)
}

// requireArtifactSuite skips the calling test when the shared harness was
// not built (env vars absent) — the same per-test t.Skip idiom
// internal/infra/storage/conformance uses for each backend leg, applied
// here to the whole self-contained suite.
func requireArtifactSuite(t *testing.T) *httptest.Server {
	t.Helper()
	if artifactServer == nil {
		t.Skip("set ELITEA_TEST_DATABASE_URL and S3_ENDPOINT_URL to run the artifact conformance suite (see deploy/docker-compose.storage-test.yml)")
	}
	return artifactServer
}

// setupArtifactSuite builds the shared harness once for the whole process
// and returns a teardown func for TestMain to run after m.Run(). It never
// fails the process: absent env vars mean every TestArtifact* test skips
// itself individually via requireArtifactSuite, exactly like S4's
// conformance package already does for its own three backend legs.
func setupArtifactSuite() (teardown func()) {
	noop := func() {}

	databaseURL := testpg.DSN(os.Getenv(artifactTestDatabaseURLEnv))
	s3Endpoint := os.Getenv("S3_ENDPOINT_URL")
	if databaseURL == "" || s3Endpoint == "" {
		return noop
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, dropDB, err := newArtifactContractPool(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifact suite setup: %v\n", err)
		return noop
	}

	store, err := newArtifactContractStore(ctx)
	if err != nil {
		pool.Close()
		dropDB()
		fmt.Fprintf(os.Stderr, "artifact suite setup: %v\n", err)
		return noop
	}

	// Every request through this harness authenticates with a real bearer
	// credential validated by artifactContractValidator; RBAC still runs for
	// real (artifactPermissiveResolver grants everything unconditionally), so
	// this exercises the real auth/RBAC/handler chain end to end, the same way
	// internal/api/router_security_test.go's S11 tests do — S19 is about the
	// artifact API's own contract, not re-proving RBAC enforcement, which S11
	// already covers exhaustively.
	//
	// This previously set AUTH_DEV_MODE=true via os.Setenv, mutating global
	// process state from TestMain where t.Setenv cannot guard it. The bypass
	// is gone (ADR-0017, #260); injecting a validator is both narrower and
	// closer to what a deployment does.
	router := api.NewRouter(api.RouterConfig{
		Pool:          pool,
		AuthValidator: artifactContractValidator{},
		// Required since validatePrincipal became fail-closed: a router with a
		// token validator and no principal validator refuses EVERY request,
		// and this suite would report 401 for all of its two hundred-odd
		// assertions with the WARN line as the only clue.
		PrincipalValidator:         artifactContractPrincipalValidator{},
		ObjectStore:                storage.Instrument(store, "s3"),
		ArtifactPermissionResolver: artifactPermissiveResolver{},
	})
	artifactPool = pool
	artifactServer = httptest.NewServer(router)

	return func() {
		artifactServer.Close()
		pool.Close()
		dropDB()
	}
}

// newArtifactContractPool provisions an isolated, ephemeral database (same
// CREATE-DATABASE-per-run pattern every Postgres-integration package in
// this repo uses — no shared testutil exists, see
// internal/runtimecomposition/artifact_retention_sweep_postgres_integration_test.go)
// and applies every embedded shared migration plus the externally-owned
// centry.project/centry.notifications stubs this service never creates
// itself.
func newArtifactContractPool(ctx context.Context, databaseURL string) (pool *pgxpool.Pool, dropDB func(), err error) {
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", artifactTestDatabaseURLEnv, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("open PostgreSQL admin pool: %w", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		return nil, nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	databaseName := fmt.Sprintf("elitea_artifact_contract_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		return nil, nil, fmt.Errorf("create isolated PostgreSQL integration database: %w", err)
	}

	drop := func() {
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			fmt.Fprintf(os.Stderr, "artifact suite teardown: drop isolated PostgreSQL integration database: %v\n", err)
		}
		adminPool.Close()
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 12
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		drop()
		return nil, nil, fmt.Errorf("open isolated PostgreSQL integration database: %w", err)
	}
	if err := testPool.Ping(ctx); err != nil {
		testPool.Close()
		drop()
		return nil, nil, fmt.Errorf("ping isolated PostgreSQL integration database: %w", err)
	}

	if _, err := testPool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    owner_id INTEGER NOT NULL,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE centry.notifications (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    is_seen boolean NOT NULL,
    project_id integer NOT NULL,
    user_id integer NOT NULL,
    meta jsonb NOT NULL,
    event_type varchar NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp
);`); err != nil {
		testPool.Close()
		drop()
		return nil, nil, fmt.Errorf("stub centry.project/centry.notifications: %w", err)
	}
	if err := migrate.New(testPool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		testPool.Close()
		drop()
		return nil, nil, fmt.Errorf("apply embedded shared migrations: %w", err)
	}

	return testPool, drop, nil
}

// newArtifactContractStore builds a real S3 backend against RustFS,
// provisioning its one physical bucket exactly like
// internal/infra/storage/conformance's setupS3 does — this suite deliberately
// does not go through storage.ConfigFromEnv (S5), which selects one backend
// for production use; it needs the same raw S3-config-by-env-var
// construction the conformance package already established.
func newArtifactContractStore(ctx context.Context) (storage.ObjectStore, error) {
	endpoint := os.Getenv("S3_ENDPOINT_URL")
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	forcePathStyle := os.Getenv("S3_FORCE_PATH_STYLE") == "true"

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	raw := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = forcePathStyle
	})
	if _, err := raw.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: aws.String(artifactContractBucket)}); err != nil {
		var alreadyOwned *s3types.BucketAlreadyOwnedByYou
		var alreadyExists *s3types.BucketAlreadyExists
		if !errors.As(err, &alreadyOwned) && !errors.As(err, &alreadyExists) {
			return nil, fmt.Errorf("create bucket: %w", err)
		}
	}

	return s3.New(ctx, s3.Config{
		Endpoint:       endpoint,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		Region:         region,
		ForcePathStyle: forcePathStyle,
		Bucket:         artifactContractBucket,
	})
}

// artifactPermissiveResolver grants every artifact permission unconditionally
// — S19 is testing the artifact API's own contract (response shapes, status
// codes, error envelope), not RBAC enforcement, which S11's router-level
// tests already cover exhaustively for every one of these routes.
// artifactContractValidator accepts the fixed bearer token this suite sends
// (see artifactAuthToken) and returns a fixed principal, replacing the removed
// AUTH_DEV_MODE bypass (ADR-0017). Requests still traverse the real Auth
// middleware: header parsing, validation, and PrincipalValidator all run.
type artifactContractValidator struct{}

func (artifactContractValidator) ValidateToken(_ context.Context, token string) (platformauth.User, error) {
	// Compare rather than accept anything: a helper that sends the wrong
	// credential should fail here, not authenticate silently and leave the
	// suite green while exercising a credential path nothing validates.
	if token != artifactAuthToken {
		return platformauth.User{}, fmt.Errorf("artifact contract suite: unexpected token %q", token)
	}
	return platformauth.User{ID: "1", UserID: "1", Email: "contract@test.local", AuthType: "token"}, nil
}

// artifactContractPrincipalValidator passes the authenticated principal
// through. It is the counterpart to artifactContractValidator above: the token
// validator says WHO the caller is, and this says the caller is still active.
// A real deployment reads that from the database; this suite has no user rows
// and its subject is the artifact surface, not principal lifecycle.
type artifactContractPrincipalValidator struct{}

func (artifactContractPrincipalValidator) ValidatePrincipal(_ context.Context, user platformauth.User) (platformauth.User, error) {
	return user, nil
}

type artifactPermissiveResolver struct{}

func (artifactPermissiveResolver) ResolvePermissions(context.Context, platformauth.User, string, string) (platformauth.PermissionResolution, error) {
	return platformauth.PermissionResolution{
		Permissions: []string{
			"configuration.artifacts.artifacts.view",
			"configuration.artifacts.artifacts.create",
			"configuration.artifacts.artifacts.edit",
			"configuration.artifacts.artifacts.delete",
		},
	}, nil
}
