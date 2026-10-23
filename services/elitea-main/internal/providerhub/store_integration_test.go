package providerhub_test

// The admission plane's writes against a real PostgreSQL with migration 0107
// applied — a fresh database per test, dropped after. Gated on
// ELITEA_TEST_DATABASE_URL, the way the service's other integration tests
// are; without it the test skips loudly.

import (
	"context"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

func admissionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL admission-plane test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	t.Cleanup(adminPool.Close)
	databaseName := fmt.Sprintf("elitea_providerhub_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, databaseName)
		_, _ = adminPool.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+quoted)
	})
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// 0107 creates the plane; 0171 widens its origin CHECK to the gRPC
	// listener the registrar records (grpcs://host:port).
	for _, file := range []string{"0107_provider_admitted_revisions.sql", "0171_provider_origin_grpcs.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", "shared", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	return pool
}

// The facades reach their providers over gRPC, and the registrar records the
// listener it dialled, grpcs://host:port. 0107's CHECK admitted only http(s),
// so every boot-time registration was refused with 23514 and the admission
// plane listed nothing; 0171 admits the gRPC origin and still refuses an
// origin with a path or another scheme.
func TestRegisterAdmitsTheGRPCListenerOriginAndStillRefusesAPath(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	manifest := []byte(`{"name": "wikis", "provided_toolkits": []}`)
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{ProjectID: 1, ProviderID: "wikis", Origin: "grpcs://elitea-deepwiki:9443", Manifest: manifest, Actor: "facade:deepwiki"}); err != nil {
		t.Fatalf("a grpcs origin was refused: %v", err)
	}
	var origin string
	_ = pool.QueryRow(ctx, `SELECT origin FROM provider_hub.provider_origin_registration WHERE project_id = 1 AND provider_id = 'wikis'`).Scan(&origin)
	if origin != "grpcs://elitea-deepwiki:9443" {
		t.Fatalf("origin = %q", origin)
	}
	for _, refused := range []string{"grpcs://elitea-deepwiki:9443/elitea.subapp.v1.ProviderService", "grpc://elitea-deepwiki:9443", "tcp://elitea-deepwiki:9443"} {
		if _, err := providerhub.Register(ctx, pool, providerhub.Registration{ProjectID: 2, ProviderID: "wikis", Origin: refused, Manifest: manifest, Actor: "facade:deepwiki"}); err == nil {
			t.Fatalf("origin %q was admitted", refused)
		}
	}
}

func TestPresentTellsAMigratedDatabaseFromABareOne(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	if !providerhub.Present(ctx, pool) {
		t.Fatal("0107 applied, yet the plane is reported absent")
	}
	if _, err := pool.Exec(ctx, "DROP SCHEMA provider_hub CASCADE"); err != nil {
		t.Fatal(err)
	}
	if providerhub.Present(ctx, pool) {
		t.Fatal("the plane is reported present after its schema was dropped")
	}
}

func TestRegisterFilesOriginManifestAndAnInactiveRevisionOnce(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	manifest := []byte(`{"name": "deepwiki", "provided_toolkits": []}`)
	first, err := providerhub.Register(ctx, pool, providerhub.Registration{ProjectID: 1, ProviderID: "deepwiki", Origin: "https://elitea-deepwiki-svc:8443", Manifest: manifest, Actor: "facade:deepwiki"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "inactive" || first.Reason != providerhub.InactiveReason || first.ManifestDigest != providerhub.Digest(manifest) {
		t.Fatalf("%+v", first)
	}
	var publishedAt time.Time
	_ = pool.QueryRow(ctx, `SELECT published_at FROM provider_hub.provider_published_manifest WHERE digest = $1`, first.ManifestDigest).Scan(&publishedAt)
	time.Sleep(20 * time.Millisecond)
	// The same bytes again: one manifest row, one revision row, a refreshed
	// origin — and a corrected origin lands without a new manifest.
	second, err := providerhub.Register(ctx, pool, providerhub.Registration{ProjectID: 1, ProviderID: "deepwiki", Origin: "https://elitea-deepwiki-svc:9443", Manifest: manifest, Actor: "operator@autotest.local"})
	if err != nil || second.RevisionID != first.RevisionID {
		t.Fatalf("%+v %v", second, err)
	}
	// INSERT-ONLY: the manifest row is untouched by a republish, so nothing
	// an admission cites can change under it — not even its timestamp.
	var republishedAt time.Time
	_ = pool.QueryRow(ctx, `SELECT published_at FROM provider_hub.provider_published_manifest WHERE digest = $1`, first.ManifestDigest).Scan(&republishedAt)
	if !republishedAt.Equal(publishedAt) {
		t.Fatalf("the manifest row was rewritten: %s → %s", publishedAt, republishedAt)
	}
	var manifests, revisions int
	var origin, registeredBy string
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_published_manifest`).Scan(&manifests)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_admitted_revision`).Scan(&revisions)
	_ = pool.QueryRow(ctx, `SELECT origin, registered_by FROM provider_hub.provider_origin_registration WHERE project_id = 1 AND provider_id = 'deepwiki'`).Scan(&origin, &registeredBy)
	if manifests != 1 || revisions != 1 || origin != "https://elitea-deepwiki-svc:9443" || registeredBy != "operator@autotest.local" {
		t.Fatalf("manifests=%d revisions=%d origin=%s by=%s", manifests, revisions, origin, registeredBy)
	}
	// A changed manifest is a second manifest and a second revision.
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{ProjectID: 1, ProviderID: "deepwiki", Origin: origin, Manifest: []byte(`{"name": "deepwiki", "provided_toolkits": [{"name": "Wikis"}]}`), Actor: "facade:deepwiki"}); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_published_manifest`).Scan(&manifests)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_admitted_revision`).Scan(&revisions)
	if manifests != 2 || revisions != 2 {
		t.Fatalf("manifests=%d revisions=%d", manifests, revisions)
	}
}

func TestLatestAdmissionIsWhatTheRequestPathObeys(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()

	// Nobody registered: not found, and NOT an error. The gate reads this as
	// "allow", so conflating it with a failure would refuse every request in
	// the seconds between a boot and its first registration.
	if _, found, err := providerhub.LatestAdmission(ctx, pool, 1, "wikis"); found || err != nil {
		t.Fatalf("an unregistered provider: found=%v err=%v", found, err)
	}

	first := []byte(`{"name": "wikis", "provided_toolkits": []}`)
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 1, ProviderID: "wikis", Origin: "https://elitea-deepwiki:8080",
		Manifest: first, Actor: "facade:deepwiki"}); err != nil {
		t.Fatal(err)
	}
	latest, found, err := providerhub.LatestAdmission(ctx, pool, 1, "wikis")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if latest.Status != "inactive" || latest.Reason != providerhub.InactiveReason ||
		latest.ManifestDigest != providerhub.Digest(first) {
		t.Fatalf("%+v", latest)
	}

	// The registrar's own project/provider scoping: a registration under a
	// different project, and one under a different provider, are invisible
	// here. Both are read on every invoke, so a leak either way would admit
	// or refuse the wrong provider.
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 2, ProviderID: "wikis", Origin: "https://elsewhere:8080",
		Manifest: []byte(`{"name": "wikis", "other": true}`), Actor: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := providerhub.LatestAdmission(ctx, pool, 1, "inventory"); found {
		t.Fatal("a provider nobody registered under project 1 was found")
	}

	// REVOKED, the way the administration surface does it.
	if _, err := pool.Exec(ctx, `
UPDATE provider_hub.provider_admitted_revision
   SET status = 'revoked', reason = 'turned off for the incident',
       revoked_at = clock_timestamp(), revoked_by = 'operator@autotest.local'
 WHERE project_id = 1 AND provider_id = 'wikis'`); err != nil {
		t.Fatal(err)
	}
	latest, found, err = providerhub.LatestAdmission(ctx, pool, 1, "wikis")
	if err != nil || !found || latest.Status != "revoked" || latest.Reason != "turned off for the incident" {
		t.Fatalf("after a revoke: %+v found=%v err=%v", latest, found, err)
	}
	// Project 2 is untouched by project 1's revocation.
	if other, _, _ := providerhub.LatestAdmission(ctx, pool, 2, "wikis"); other.Status != "inactive" {
		t.Fatalf("project 2 read %q", other.Status)
	}

	// A NEW manifest is a new revision, and the newest one wins — this is the
	// only way a revoked provider comes back, because re-registering the same
	// bytes lands on the same revision_id and the upsert does not touch
	// `status`. The E2E journey's restore leg depends on exactly this.
	second := []byte(`{"name": "wikis", "provided_toolkits": [{"name": "Wikis"}]}`)
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 1, ProviderID: "wikis", Origin: "https://elitea-deepwiki:8080",
		Manifest: second, Actor: "operator@autotest.local"}); err != nil {
		t.Fatal(err)
	}
	latest, _, err = providerhub.LatestAdmission(ctx, pool, 1, "wikis")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "inactive" || latest.ManifestDigest != providerhub.Digest(second) {
		t.Fatalf("the newest revision is %+v", latest)
	}
	var revisions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_admitted_revision
                             WHERE project_id = 1 AND provider_id = 'wikis'`).Scan(&revisions)
	if revisions != 2 {
		t.Fatalf("%d revisions for project 1; the revoked one must survive as audit", revisions)
	}

	// THE TIE-BREAK. Both revisions stamped with the same admitted_at is the
	// ambiguous case the ORDER BY exists for, and it must resolve to the
	// closed answer rather than to whichever row the planner returns.
	if _, err := pool.Exec(ctx, `
UPDATE provider_hub.provider_admitted_revision SET admitted_at = TIMESTAMPTZ '2026-01-01 00:00:00Z'
 WHERE project_id = 1 AND provider_id = 'wikis'`); err != nil {
		t.Fatal(err)
	}
	if tied, _, _ := providerhub.LatestAdmission(ctx, pool, 1, "wikis"); tied.Status != "revoked" {
		t.Fatalf("a tie resolved to %q, want the refusal", tied.Status)
	}
}

func TestRecordHealthOverwritesTheProjectionAndNeedsARegistration(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	if err := providerhub.RecordHealth(ctx, pool, 1, "ghost", true, "never registered"); err == nil {
		t.Fatal("a probe of an unregistered provider wrote a row")
	}
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{ProjectID: 1, ProviderID: "deepwiki", Origin: "https://p", Manifest: []byte(`{"name":"deepwiki"}`), Actor: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := providerhub.RecordHealth(ctx, pool, 1, "deepwiki", false, "HTTP 503"); err != nil {
		t.Fatal(err)
	}
	if err := providerhub.RecordHealth(ctx, pool, 1, "deepwiki", true, "health UP in 3ms"); err != nil {
		t.Fatal(err)
	}
	var rows int
	var healthy bool
	var detail string
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_health_projection`).Scan(&rows)
	_ = pool.QueryRow(ctx, `SELECT healthy, detail FROM provider_hub.provider_health_projection WHERE project_id = 1 AND provider_id = 'deepwiki'`).Scan(&healthy, &detail)
	if rows != 1 || !healthy || detail != "health UP in 3ms" {
		t.Fatalf("rows=%d healthy=%v detail=%q", rows, healthy, detail)
	}
}

// The registrar re-files its own revision every tick and the upsert bumps
// admitted_at, so "latest by time" is the registrar's row minutes after an
// operator activated a different one. The active revision must win: the
// listing, the gate and the deactivate target all read this function.
// MEASURED by DWIKI-013c: the page showed Inactive with an Activate control
// beside a revision that was in force.
func TestAnActiveRevisionOutranksALaterReFiledInactiveOne(t *testing.T) {
	// overlayPool, not admissionPool: activation needs 0109, and a skip on
	// its absence made this test pass under mutation on its first run —
	// the absence-reads-as-correctness trap, one more time.
	pool := overlayPool(t)
	ctx := context.Background()
	registrars := providerhub.Registration{
		ProjectID: 7, ProviderID: "wikis", Origin: "https://elitea-deepwiki:8080",
		Manifest: []byte(`{"name": "wikis", "provided_toolkits": [], "from": "registrar"}`), Actor: "facade:deepwiki"}
	if _, err := providerhub.Register(ctx, pool, registrars); err != nil {
		t.Fatal(err)
	}
	// An operator readmits a reviewed manifest (a different digest, so a
	// different revision) and activates it.
	readmitted, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 7, ProviderID: "wikis", Origin: "https://elitea-deepwiki:8080",
		Manifest: []byte(`{"name": "wikis", "provided_toolkits": [], "from": "operator"}`), Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providerhub.Activate(ctx, pool, providerhub.ActivateRequest{
		ProjectID: 7, ProviderID: "wikis", ExpectedDigest: readmitted.ManifestDigest,
		Body: []byte(`{"egress_profile":"provider-only"}`), Reason: "reviewed", Actor: "admin"}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	// The registrar's next tick re-files ITS revision, which bumps admitted_at.
	if _, err := providerhub.Register(ctx, pool, registrars); err != nil {
		t.Fatal(err)
	}
	latest, found, err := providerhub.LatestAdmission(ctx, pool, 7, "wikis")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if latest.Status != "active" || latest.RevisionID != readmitted.RevisionID {
		t.Fatalf("the re-filed inactive revision outranked the active one: %+v", latest)
	}
}
