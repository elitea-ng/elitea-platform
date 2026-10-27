package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

const integrationPolicy = `{"schema":"elitea.credential-policy.v1","allowed_identity_modes":["dedicated_agent"],"vault_required_credential_classes":[],"max_mandate_ttl_seconds":3600,"audit_backlog_alert_threshold":100}`

func putIntegrationRevision(t *testing.T, ctx context.Context, store *CredentialGovernanceStore, id string) profile.PolicyRevision {
	t.Helper()
	doc, canonical, err := profile.ParsePolicyDocument([]byte(integrationPolicy))
	if err != nil {
		t.Fatal(err)
	}
	revision := profile.PolicyRevision{ID: id, Digest: profile.Digest(canonical), Document: doc, Canonical: canonical, CreatedBy: "integration"}
	if err := store.PutRevision(ctx, revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func pinInTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, admission profile.Admission, pin admissionPin, commit bool) {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pinExecutionAdmission(ctx, tx, admission, pin); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if commit {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresCredentialGovernanceProfile covers migration 0180 and the
// store: immutable revisions, the boot digest check, immutable pins, the
// rollback of a pin with its admission, and the continuation guard.
func TestPostgresCredentialGovernanceProfile(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	revision := putIntegrationRevision(t, ctx, store, "it-r1")
	if err := store.PutRevision(ctx, revision); !errors.Is(err, profile.ErrRevisionExists) {
		t.Fatalf("second put: err = %v, want ErrRevisionExists", err)
	}
	read, err := store.Revision(ctx, "it-r1")
	if err != nil || read.Digest != revision.Digest || read.VerifyDigest() != nil {
		t.Fatalf("read revision: %+v err=%v", read, err)
	}
	if _, err := store.Revision(ctx, "it-missing"); !errors.Is(err, profile.ErrRevisionNotFound) {
		t.Fatalf("missing revision: err = %v", err)
	}

	// Revisions reject UPDATE and DELETE.
	for _, statement := range []string{
		`UPDATE elitea_identity.credential_policy_revisions SET created_by = 'x' WHERE id = 'it-r1'`,
		`DELETE FROM elitea_identity.credential_policy_revisions WHERE id = 'it-r1'`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s must fail", statement)
		}
	}

	// Boot against the database: a known revision governs; an unknown one
	// refuses.
	on := profile.Config{Enabled: true, ActivePolicyRevisionID: "it-r1"}
	governed, _, err := profile.Boot(ctx, on, store, store, nil)
	if err != nil || !governed.Governed() {
		t.Fatalf("boot: %+v err=%v", governed, err)
	}
	if _, _, err := profile.Boot(ctx, profile.Config{Enabled: true, ActivePolicyRevisionID: "it-r9"}, store, store, nil); !errors.Is(err, profile.ErrBootRefused) {
		t.Fatalf("unknown revision: err = %v", err)
	}

	// No pin row means legacy, and a fresh deployment has no guard.
	if found, err := store.AnyGovernedPin(ctx); err != nil || found {
		t.Fatalf("probe on empty table: %v %v", found, err)
	}
	pin, err := store.ReadPin(ctx, "it-none")
	if err != nil || pin.Profile != profile.ProfileLegacy {
		t.Fatalf("absent pin: %+v err=%v", pin, err)
	}

	// A pin rolled back with its admission leaves no row.
	pinInTx(t, ctx, pool, governed, admissionPin{executionID: "it-rolled-back"}, false)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_admission_profiles WHERE execution_id = 'it-rolled-back'`)

	scope := "agent-conversation:it"
	pinInTx(t, ctx, pool, governed, admissionPin{executionID: "it-run", continuationScope: scope}, true)
	pin, err = store.ReadPin(ctx, "it-run")
	if err != nil || pin.Profile != profile.ProfileGoverned || pin.PolicyRevisionID != "it-r1" || pin.PolicyDigest != revision.Digest {
		t.Fatalf("governed pin: %+v err=%v", pin, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_admission_profiles SET profile = 'elitea.credential-profile.legacy.v1', policy_revision_id = NULL, policy_digest = NULL WHERE execution_id = 'it-run'`); err == nil {
		t.Fatal("a pin must never be updated (no downgrade)")
	}
	// Re-admitting the same execution keeps its first pin.
	pinInTx(t, ctx, pool, governed, admissionPin{executionID: "it-run", continuationScope: scope}, true)

	// The flag is turned off: the next boot finds governed pins and guards.
	legacy, _, err := profile.Boot(ctx, profile.Config{}, store, store, nil)
	if err != nil || legacy.Governed() || !legacy.ContinuationGuard() {
		t.Fatalf("flag-off boot after governed work: %+v err=%v", legacy, err)
	}
	// A resume of the governed run stays governed on its original revision.
	pinInTx(t, ctx, pool, legacy, admissionPin{executionID: "it-resume", continuationScope: scope, continuation: true}, true)
	pin, err = store.ReadPin(ctx, "it-resume")
	if err != nil || pin.Profile != profile.ProfileGoverned || pin.PolicyRevisionID != "it-r1" {
		t.Fatalf("resume after flag off: %+v err=%v", pin, err)
	}
	// A fresh legacy admission and a continuation of a legacy conversation
	// write nothing.
	pinInTx(t, ctx, pool, legacy, admissionPin{executionID: "it-fresh", continuationScope: scope}, true)
	pinInTx(t, ctx, pool, legacy, admissionPin{executionID: "it-other", continuationScope: "agent-conversation:other", continuation: true}, true)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_admission_profiles WHERE execution_id IN ('it-fresh', 'it-other')`)
}

// TestPostgresGovernedToolRunAdmissionPins is the end-to-end admission
// check: one governed admission writes exactly one pin in its transaction,
// and the same admission with the flag off writes none.
func TestPostgresGovernedToolRunAdmissionPins(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	legacyRepository, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newToolRunAdmissionService(t, legacyRepository, "legacy").Submit(ctx, toolRunSubmitRequest("legacy-run", `{"repo":"a"}`)); err != nil {
		t.Fatal(err)
	}
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_admission_profiles`)

	revision := putIntegrationRevision(t, ctx, store, "it-tool")
	policy := toolRunDispatchPolicy()
	policy.CredentialProfile = profile.GovernedAdmission(revision)
	governedRepository, err := NewToolkitCallToolJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newToolRunAdmissionService(t, governedRepository, "governed").Submit(ctx, toolRunSubmitRequest("governed-run", `{"repo":"b"}`))
	if err != nil || !admitted.Outcome.Created {
		t.Fatalf("governed admission: %+v err=%v", admitted.Outcome, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_admission_profiles WHERE profile = 'elitea.credential-profile.governed.v1' AND policy_revision_id = 'it-tool'`)
}
