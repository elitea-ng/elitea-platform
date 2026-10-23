package providerhub_test

// ACTIVATION against a real PostgreSQL with 0107 AND 0109 applied.
//
// Two kinds of claim live here and they are not interchangeable:
//
//	the SCHEMA's — the foreign key rejects an overlay revision nobody issued,
//	  and the partial unique index rejects a second active revision. These are
//	  asserted through raw SQL as well as through the store, because a
//	  constraint that only the Go path respects is a constraint the next Go
//	  path can forget.
//	the STORE's — the compare-and-swap on the manifest digest, the typed
//	  outcomes, and the reuse of an overlay by digest.
//
// The first set is why 0109 exists at all. Before it, `overlay_revision` was a
// free TEXT column, so 0107's "activation requires a reviewed policy overlay"
// really said "requires a non-empty string" and `UPDATE … SET status='active',
// overlay_revision='todo'` would have passed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

const (
	overlayProject  = int64(41)
	overlayProvider = "wikis"
	overlayActor    = "operator@autotest.local"
)

// overlayPool is admissionPool with 0109 on top.
func overlayPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := admissionPool(t)
	sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", "shared", "0109_provider_policy_overlay.sql"))
	if err != nil {
		t.Fatalf("read 0109: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply 0109: %v", err)
	}
	return pool
}

// registerForActivation files the origin, manifest and inactive revision an
// activation acts on, through the production writer rather than by hand: a
// fixture that inserted its own rows could satisfy shapes Register does not
// produce.
func registerForActivation(t *testing.T, pool *pgxpool.Pool, manifest string) providerhub.Admitted {
	t.Helper()
	admitted, err := providerhub.Register(context.Background(), pool, providerhub.Registration{
		ProjectID:  overlayProject,
		ProviderID: overlayProvider,
		Origin:     "https://elitea-deepwiki:8443",
		Manifest:   []byte(manifest),
		Actor:      overlayActor,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return admitted
}

func activateRequest(admitted providerhub.Admitted, overlay string) providerhub.ActivateRequest {
	return providerhub.ActivateRequest{
		ProjectID:      overlayProject,
		ProviderID:     overlayProvider,
		ExpectedDigest: admitted.ManifestDigest,
		Body:           []byte(overlay),
		Reason:         "reviewed the wikis toolkit; read-only, 30s timeout",
		Actor:          overlayActor,
	}
}

/* ── the schema's claims ────────────────────────────────────────────────── */

// A FABRICATED overlay revision is rejected by PostgreSQL.
//
// THE ASSERTION 0109 EXISTS FOR. Run this against the tree before 0109 and the
// UPDATE succeeds: 0107's CHECK is satisfied by any non-empty string, so an
// "activated" provider could cite a policy that was never written, never
// reviewed and never stored. The failure has to come from the database, because
// the point is that no Go path can route around it.
func TestAFabricatedOverlayRevisionCannotActivateARevision(t *testing.T) {
	pool := overlayPool(t)
	admitted := registerForActivation(t, pool, `{"name":"wikis"}`)

	_, err := pool.Exec(context.Background(), `
UPDATE provider_hub.provider_admitted_revision
   SET status = 'active', overlay_revision = 'lpo_00000000000000000000000000000000'
 WHERE revision_id = $1`, admitted.RevisionID)
	if err == nil {
		t.Fatal("a revision was activated against an overlay revision nobody issued; " +
			"the foreign key 0109 adds is what makes overlay_revision mean anything")
	}
	// And the row did not move.
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM provider_hub.provider_admitted_revision WHERE revision_id = $1`,
		admitted.RevisionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "inactive" {
		t.Errorf("status = %q after a refused activation, want inactive", status)
	}
}

// The overlay row itself cannot cite a manifest nobody published.
func TestAnOverlayCannotBeIssuedAgainstAnUnpublishedManifest(t *testing.T) {
	pool := overlayPool(t)
	registerForActivation(t, pool, `{"name":"wikis"}`)

	_, err := pool.Exec(context.Background(), `
INSERT INTO provider_hub.provider_policy_overlay
  (overlay_revision, project_id, provider_id, manifest_digest, body, digest, created_by, approved_by)
VALUES ('lpo_ffffffffffffffffffffffffffffffff', $1, $2,
        'dead00000000000000000000000000000000000000000000000000000000beef',
        '{}'::jsonb, 'ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff',
        $3, $3)`, overlayProject, overlayProvider, overlayActor)
	if err == nil {
		t.Fatal("an overlay was bound to a manifest digest that was never published; " +
			"a reviewed policy over unreviewed bytes is the state the reference prevents")
	}
}

// The id must abbreviate the digest beside it. Without the CHECK a row could
// carry an id derived from one body and a digest from another, and every later
// verification would compare the wrong pair.
func TestAnOverlayIdMustAbbreviateItsOwnDigest(t *testing.T) {
	pool := overlayPool(t)
	admitted := registerForActivation(t, pool, `{"name":"wikis"}`)

	_, err := pool.Exec(context.Background(), `
INSERT INTO provider_hub.provider_policy_overlay
  (overlay_revision, project_id, provider_id, manifest_digest, body, digest, created_by, approved_by)
VALUES ('lpo_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', $1, $2, $3, '{}'::jsonb,
        'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', $4, $4)`,
		overlayProject, overlayProvider, admitted.ManifestDigest, overlayActor)
	if err == nil {
		t.Fatal("an overlay id that abbreviates a different digest was accepted")
	}
}

/* ── the store's claims ─────────────────────────────────────────────────── */

// The happy path, end to end: an overlay is issued, the revision goes active,
// and the row satisfies both of 0107's architectural constraints.
func TestActivationIssuesAnOverlayAndPutsTheRevisionInForce(t *testing.T) {
	pool := overlayPool(t)
	admitted := registerForActivation(t, pool, `{"name":"wikis","provided_toolkits":[{"name":"wikis"}]}`)

	activated, err := providerhub.Activate(context.Background(), pool,
		activateRequest(admitted, `{"timeouts":{"invoke_seconds":30},"egress_profile":"provider-only"}`))
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if activated.Status != "active" {
		t.Errorf("status = %q, want active", activated.Status)
	}
	if activated.RevisionID != admitted.RevisionID {
		t.Errorf("activated %q, want the registered revision %q", activated.RevisionID, admitted.RevisionID)
	}

	ctx := context.Background()
	var status, overlayRevision, manifestDigest, reason string
	if err := pool.QueryRow(ctx, `
SELECT status, overlay_revision, manifest_digest, reason
  FROM provider_hub.provider_admitted_revision WHERE revision_id = $1`,
		admitted.RevisionID).Scan(&status, &overlayRevision, &manifestDigest, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "active" || overlayRevision != activated.OverlayRevision {
		t.Fatalf("stored row is status=%q overlay=%q, want active with the issued overlay",
			status, overlayRevision)
	}
	// The reason is the OPERATOR's. Registration's InactiveReason must not
	// survive an activation: a row that reads "recorded, not in force" while
	// its status says active is the worst of both.
	if reason != "reviewed the wikis toolkit; read-only, 30s timeout" {
		t.Errorf("reason = %q, want the operator's sentence", reason)
	}

	// The overlay row records both actors and binds to the reviewed manifest.
	var createdBy, approvedBy, boundDigest string
	var body []byte
	if err := pool.QueryRow(ctx, `
SELECT created_by, approved_by, manifest_digest, body::text
  FROM provider_hub.provider_policy_overlay WHERE overlay_revision = $1`,
		activated.OverlayRevision).Scan(&createdBy, &approvedBy, &boundDigest, &body); err != nil {
		t.Fatal(err)
	}
	if boundDigest != admitted.ManifestDigest {
		t.Errorf("the overlay is bound to %q, want the manifest it was reviewed against", boundDigest)
	}
	// v1 records both and enforces nothing about them. This assertion is the
	// DECISION written down: a single-operator deployment must be able to
	// activate, and the columns are what make later enforcement a policy change
	// rather than a migration over live rows.
	if createdBy != overlayActor || approvedBy != overlayActor {
		t.Errorf("created_by/approved_by = %q/%q, want the acting operator recorded in both",
			createdBy, approvedBy)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the stored overlay is not an object: %v", err)
	}
	if _, present := decoded["egress_profile"]; !present {
		t.Error("the reviewed facts the caller sent were not stored verbatim")
	}
}

// The SAME reviewed facts issue ONE overlay row, whatever order they arrive in.
// That is what makes a content-derived revision id meaningful.
func TestTheSameReviewedFactsAreOneOverlayRevision(t *testing.T) {
	pool := overlayPool(t)
	admitted := registerForActivation(t, pool, `{"name":"wikis"}`)
	ctx := context.Background()

	first, err := providerhub.Activate(ctx, pool,
		activateRequest(admitted, `{"egress_profile":"provider-only","limits_profile":"small"}`))
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := providerhub.Deactivate(ctx, pool, overlayProject, overlayProvider, "paused"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	// Same facts, keys reordered and whitespace added.
	second, err := providerhub.Activate(ctx, pool,
		activateRequest(admitted, "{\n  \"limits_profile\": \"small\",\n  \"egress_profile\": \"provider-only\"\n}"))
	if err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if first.OverlayRevision != second.OverlayRevision {
		t.Errorf("the same reviewed facts issued two revisions (%s, %s)",
			first.OverlayRevision, second.OverlayRevision)
	}
	var overlays int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_hub.provider_policy_overlay`).Scan(&overlays); err != nil {
		t.Fatal(err)
	}
	if overlays != 1 {
		t.Errorf("%d overlay rows for one set of reviewed facts, want 1", overlays)
	}
}

// THE COMPARE-AND-SWAP. A manifest that changed since the review is refused.
func TestActivationRefusesAManifestThatMovedSinceTheReview(t *testing.T) {
	pool := overlayPool(t)
	admitted := registerForActivation(t, pool, `{"name":"wikis","version":1}`)

	request := activateRequest(admitted, `{}`)
	request.ExpectedDigest = providerhub.Digest([]byte(`{"name":"wikis","version":2}`))

	_, err := providerhub.Activate(context.Background(), pool, request)
	if err == nil {
		t.Fatal("a revision was activated against a digest the operator did not review")
	}
	if !errors.Is(err, providerhub.ErrManifestDigestMismatch) {
		t.Fatalf("err = %v, want ErrManifestDigestMismatch", err)
	}
	// And no overlay was issued for a refused activation: a policy row nobody
	// activated against is a reviewed statement with no subject.
	var overlays int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_hub.provider_policy_overlay`).Scan(&overlays); err != nil {
		t.Fatal(err)
	}
	if overlays != 0 {
		t.Errorf("%d overlays issued by a refused activation, want 0", overlays)
	}
}

// A REVOKED revision is never re-activated.
func TestARevokedRevisionCannotBeActivated(t *testing.T) {
	pool := overlayPool(t)
	admitted := registerForActivation(t, pool, `{"name":"wikis"}`)
	if _, err := pool.Exec(context.Background(), `
UPDATE provider_hub.provider_admitted_revision
   SET status = 'revoked', revoked_at = clock_timestamp(), revoked_by = $2, reason = 'decommissioned'
 WHERE revision_id = $1`, admitted.RevisionID, overlayActor); err != nil {
		t.Fatal(err)
	}

	_, err := providerhub.Activate(context.Background(), pool, activateRequest(admitted, `{}`))
	if !errors.Is(err, providerhub.ErrRevisionNotInactive) {
		t.Fatalf("err = %v, want ErrRevisionNotInactive — a revocation is a decision "+
			"about those bytes, and undoing it must go back through registration", err)
	}
}

// A SECOND active revision meets 0107's partial unique index, and the store
// reports it as a typed conflict rather than as a raw 23505.
func TestASecondActiveRevisionIsRefusedAsAConflict(t *testing.T) {
	pool := overlayPool(t)
	ctx := context.Background()
	first := registerForActivation(t, pool, `{"name":"wikis","version":1}`)
	if _, err := providerhub.Activate(ctx, pool, activateRequest(first, `{}`)); err != nil {
		t.Fatalf("activate the first revision: %v", err)
	}
	// A second registration of DIFFERENT bytes is a second revision of the same
	// provider in the same project.
	second := registerForActivation(t, pool, `{"name":"wikis","version":2}`)
	if second.RevisionID == first.RevisionID {
		t.Fatal("the fixture produced one revision; the conflict cannot arise")
	}

	request := activateRequest(second, `{}`)
	request.RevisionID = second.RevisionID
	_, err := providerhub.Activate(ctx, pool, request)
	if !errors.Is(err, providerhub.ErrAnotherRevisionActive) {
		t.Fatalf("err = %v, want ErrAnotherRevisionActive: two active revisions would make "+
			"\"which manifest is this deployment running\" unanswerable", err)
	}
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_hub.provider_admitted_revision WHERE status = 'active'`).
		Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Errorf("%d active revisions, want exactly 1", active)
	}
}

// Deactivation returns the revision to `inactive` and KEEPS the overlay
// reference. 0107's CHECK constrains the active state only, so nothing has to
// be NULLed — and the retained reference is what tells a later reader which
// policy this provider was last in force under.
func TestDeactivationKeepsTheOverlayItWasActivatedUnder(t *testing.T) {
	pool := overlayPool(t)
	ctx := context.Background()
	admitted := registerForActivation(t, pool, `{"name":"wikis"}`)
	activated, err := providerhub.Activate(ctx, pool, activateRequest(admitted, `{"limits_profile":"small"}`))
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	if err := providerhub.Deactivate(ctx, pool, overlayProject, overlayProvider,
		"paused pending a security review"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	var status, reason string
	var overlay *string
	if err := pool.QueryRow(ctx, `
SELECT status, reason, overlay_revision FROM provider_hub.provider_admitted_revision
 WHERE revision_id = $1`, admitted.RevisionID).Scan(&status, &reason, &overlay); err != nil {
		t.Fatal(err)
	}
	if status != "inactive" {
		t.Errorf("status = %q, want inactive", status)
	}
	if overlay == nil || *overlay != activated.OverlayRevision {
		t.Errorf("overlay_revision = %v, want the one it ran under kept", overlay)
	}
	if reason != "paused pending a security review" {
		t.Errorf("reason = %q, want the operator's", reason)
	}

	// Deactivate is NOT revoke: no revocation columns were filled in, so a row
	// that was paused cannot be mistaken for one that was retired.
	var revokedBy *string
	if err := pool.QueryRow(ctx,
		`SELECT revoked_by FROM provider_hub.provider_admitted_revision WHERE revision_id = $1`,
		admitted.RevisionID).Scan(&revokedBy); err != nil {
		t.Fatal(err)
	}
	if revokedBy != nil {
		t.Errorf("revoked_by = %q after a deactivate; pausing is not retiring", *revokedBy)
	}
}

func TestDeactivatingWhatIsNotActiveIsNotReportedAsDone(t *testing.T) {
	pool := overlayPool(t)
	registerForActivation(t, pool, `{"name":"wikis"}`)

	err := providerhub.Deactivate(context.Background(), pool, overlayProject, overlayProvider, "stop")
	if !errors.Is(err, providerhub.ErrNoActiveRevision) {
		t.Fatalf("err = %v, want ErrNoActiveRevision", err)
	}
	err = providerhub.Deactivate(context.Background(), pool, overlayProject, "never-registered", "stop")
	if !errors.Is(err, providerhub.ErrProviderNotRegistered) {
		t.Fatalf("err = %v, want ErrProviderNotRegistered", err)
	}
}

func TestActivatingAProviderNobodyRegisteredIsRefused(t *testing.T) {
	pool := overlayPool(t)

	_, err := providerhub.Activate(context.Background(), pool, providerhub.ActivateRequest{
		ProjectID:      overlayProject,
		ProviderID:     "never-registered",
		ExpectedDigest: providerhub.Digest([]byte(`{}`)),
		Reason:         "because",
		Actor:          overlayActor,
	})
	if !errors.Is(err, providerhub.ErrProviderNotRegistered) {
		t.Fatalf("err = %v, want ErrProviderNotRegistered", err)
	}
}

// 0107 applied and 0109 not: a deployment that can record and still cannot
// activate. Reported as an absent plane, not as a query failure — the caller
// answers 501 for it, which is the answer that names the missing migration.
func TestActivationReportsAnAbsentOverlayPlane(t *testing.T) {
	pool := admissionPool(t) // 0107 only
	ctx := context.Background()
	if providerhub.OverlayPlanePresent(ctx, pool) {
		t.Fatal("the overlay plane is reported present without 0109")
	}
	admitted, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: overlayProject, ProviderID: overlayProvider,
		Origin: "https://elitea-deepwiki:8443", Manifest: []byte(`{"name":"wikis"}`), Actor: overlayActor,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = providerhub.Activate(ctx, pool, activateRequest(admitted, `{}`))
	if !errors.Is(err, providerhub.ErrOverlayPlaneAbsent) {
		t.Fatalf("err = %v, want ErrOverlayPlaneAbsent", err)
	}
}
