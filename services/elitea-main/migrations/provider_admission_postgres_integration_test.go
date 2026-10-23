package migrations_test

// The two constraints that carry the provider-admission architecture, asked of
// PostgreSQL rather than of the SQL text.
//
// WHY THIS IS AN INTEGRATION TEST AND NOT A READING. A CHECK constraint that is
// written but not enforced — a typo in the expression, a column that admits
// NULL where the check assumes a value, a partial index whose predicate never
// matches — reads exactly like one that works. The whole point of putting
// ADR-0012's overlay rule in the schema instead of in a handler is that it
// cannot be bypassed; that claim is only worth something if something tries to
// bypass it.
//
// THE FIRST TEST IS THE ARCHITECTURE. "A missing overlay policy fails
// admission" is why this deployment can record and show a descriptor and
// physically cannot activate one. If that check ever stops refusing, this
// platform gains a registration surface for a runtime that cannot police what
// it registers — and nothing else in the system would notice.

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	testDigest   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testOrigin   = "https://elitea-deepwiki:8443"
	testProvider = "deepwiki"
)

func TestAnActiveRevisionWithoutAnOverlayIsRefused(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_origin_registration
  (project_id, provider_id, origin, registered_by)
VALUES ($1, $2, $3, 'test')`, 1, testProvider, testOrigin); err != nil {
		t.Fatalf("seeding the origin: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_published_manifest (digest, manifest_bytes)
VALUES ($1, $2)`, testDigest, []byte(`{"provider":"deepwiki"}`)); err != nil {
		t.Fatalf("seeding the manifest: %v", err)
	}

	// THE ADR-0012 RULE. An overlay policy is what tells this platform how to
	// police a provider's calls; without one there is nothing to enforce, so an
	// active revision is a provider admitted with no policy at all.
	_, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_admitted_revision
  (revision_id, project_id, provider_id, manifest_digest, status, reason, admitted_by)
VALUES ('r-active-no-overlay', 1, $1, $2, 'active', 'should not be possible', 'test')`,
		testProvider, testDigest)
	if err == nil {
		t.Fatal("an ACTIVE revision with no overlay was accepted. ADR-0012 requires that " +
			"a missing overlay policy fails admission, and this constraint is the only " +
			"thing enforcing it — the platform can now admit a provider it cannot police.")
	}
	if !strings.Contains(err.Error(), "active_needs_overlay") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	// The other half: inactive with no overlay is exactly what this deployment
	// CAN do, and must keep being able to do. A constraint that refused both
	// would make the whole surface unusable while looking correct.
	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_admitted_revision
  (revision_id, project_id, provider_id, manifest_digest, status, reason, admitted_by)
VALUES ('r-inactive', 1, $1, $2, 'inactive', 'no overlay policy can be issued yet', 'test')`,
		testProvider, testDigest); err != nil {
		t.Fatalf("an INACTIVE revision with no overlay was refused, which leaves this "+
			"deployment unable to record anything at all: %v", err)
	}
}

func TestOnlyOneRevisionCanBeActivePerProvider(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_origin_registration
  (project_id, provider_id, origin, registered_by)
VALUES (1, $1, $2, 'test')`, testProvider, testOrigin); err != nil {
		t.Fatalf("seeding the origin: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_published_manifest (digest, manifest_bytes)
VALUES ($1, $2)`, testDigest, []byte(`{"provider":"deepwiki"}`)); err != nil {
		t.Fatalf("seeding the manifest: %v", err)
	}

	// THE OVERLAYS ARE REAL NOW, and they were not when this test was written.
	//
	// It used to pass 'overlay-1', 'overlay-2' and 'overlay-3' — arbitrary
	// strings — because `overlay_revision` was a free TEXT column and 0107's
	// CHECK only asked that it be non-NULL. That is precisely the weakness
	// migration 0109 closes: its FOREIGN KEY makes the column name a row that
	// records a reviewed policy, its digest and the manifest it was reviewed
	// against. The strings this test used to invent are now rejected by
	// PostgreSQL, which is the whole point, so the fixture issues three actual
	// overlay revisions instead.
	issueOverlay := func(digest string) string {
		t.Helper()
		revision := "lpo_" + digest[:32]
		if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_policy_overlay
  (overlay_revision, project_id, provider_id, manifest_digest, body, digest, created_by, approved_by)
VALUES ($1, 1, $2, $3, '{}'::jsonb, $4, 'test', 'test')`,
			revision, testProvider, testDigest, digest); err != nil {
			t.Fatalf("issuing an overlay: %v", err)
		}
		return revision
	}
	firstOverlay := issueOverlay(strings.Repeat("1", 64))
	secondOverlay := issueOverlay(strings.Repeat("2", 64))
	thirdOverlay := issueOverlay(strings.Repeat("3", 64))

	insertActive := func(id, overlay string) error {
		_, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_admitted_revision
  (revision_id, project_id, provider_id, manifest_digest, overlay_revision,
   status, reason, admitted_by)
VALUES ($1, 1, $2, $3, $4, 'active', 'admitted', 'test')`,
			id, testProvider, testDigest, overlay)
		return err
	}

	if err := insertActive("r-first", firstOverlay); err != nil {
		t.Fatalf("the first active revision was refused: %v", err)
	}
	// Two active revisions would make "which manifest is this deployment
	// running" unanswerable — and the answer would differ per query plan.
	if err := insertActive("r-second", secondOverlay); err == nil {
		t.Fatal("a SECOND active revision was accepted for the same provider")
	}

	// A revoked revision does not occupy the slot: the partial index is what
	// makes replacing an admission possible at all.
	if _, err := pool.Exec(ctx, `
UPDATE provider_hub.provider_admitted_revision
   SET status = 'revoked', reason = 'superseded',
       revoked_at = clock_timestamp(), revoked_by = 'test'
 WHERE revision_id = 'r-first'`); err != nil {
		t.Fatalf("revoking the first revision: %v", err)
	}
	if err := insertActive("r-third", thirdOverlay); err != nil {
		t.Fatalf("a new active revision was refused after the previous one was revoked, "+
			"so an admission could never be replaced: %v", err)
	}
}

func TestAPublishedManifestCannotBeEdited(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	original := []byte(`{"provider":"deepwiki","toolkits":["wikis"]}`)
	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_published_manifest (digest, manifest_bytes)
VALUES ($1, $2)`, testDigest, original); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	// Content-addressed and insert-only: an admission cites a digest, and the
	// bytes behind it must not be able to change afterwards.
	if _, err := pool.Exec(ctx, `
INSERT INTO provider_hub.provider_published_manifest (digest, manifest_bytes)
VALUES ($1, $2)`, testDigest, []byte(`{"tampered":true}`)); err == nil {
		t.Fatal("the same digest accepted different bytes")
	}

	var stored []byte
	if err := pool.QueryRow(ctx, `
SELECT manifest_bytes FROM provider_hub.provider_published_manifest WHERE digest = $1`,
		testDigest).Scan(&stored); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(stored) != string(original) {
		t.Errorf("the stored manifest changed: %s", stored)
	}
}

func TestHealthIsAProjectionAndNotAColumnOnTheRevision(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The whole reason service_descriptors.go refused: the legacy runtime's `healthy` was
	// which in-process dict an entry landed in, presented as a fact about the
	// provider. Storing it on the revision row would reproduce that in durable
	// form — a boolean with no observation time, which a reader cannot tell
	// from "nobody has asked lately".
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema = 'provider_hub'
   AND table_name   = 'provider_admitted_revision'
   AND column_name IN ('healthy', 'health', 'is_healthy')`).Scan(&count); err != nil {
		t.Fatalf("inspecting the revision columns: %v", err)
	}
	if count != 0 {
		t.Errorf("provider_admitted_revision carries %d health column(s). Health is a "+
			"projection with an observation time, not a property of an admission.", count)
	}

	// And the projection carries one, so a stale reading is distinguishable.
	var hasObservedAt bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
   WHERE table_schema = 'provider_hub'
     AND table_name   = 'provider_health_projection'
     AND column_name  = 'observed_at')`).Scan(&hasObservedAt); err != nil {
		t.Fatalf("inspecting the projection: %v", err)
	}
	if !hasObservedAt {
		t.Error("provider_health_projection has no observed_at, so a reader cannot tell " +
			"an unhealthy provider from one nobody has probed")
	}
}
