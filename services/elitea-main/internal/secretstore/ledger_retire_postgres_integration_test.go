package secretstore

// RetireSuperseded against a real PostgreSQL, with shared migrations 0182
// and 0220 applied from their FILES (#1505 review).

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newRetireLedgerPool is newLedgerPool plus 0220, which adds the ledger
// state 'retired'. 0220 also alters credential_references; a stub table
// stands in for it here.
func newRetireLedgerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newLedgerPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE elitea_identity.credential_references (id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/shared/0220_credential_reference_rotation.sql")
	if err != nil {
		t.Fatalf("the migration file must be readable: %v", err)
	}
	for range 2 { // applying it twice must be a no-op
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply 0220: %v", err)
		}
	}
	return pool
}

func TestPostgresLedgerRetireSuperseded(t *testing.T) {
	pool := newRetireLedgerPool(t)
	ledger, err := NewLedger(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	value := NewRuntimeSecret([]byte("v"))

	// Vault: a committed version is destroyed and its row moves to retired.
	vault := versionScoped{&recordingStore{}}
	if _, err := ledger.StagedWrite(ctx, vault, governedContext(), ledgerRef(), value, 0); err != nil {
		t.Fatal(err)
	}
	outcome, err := ledger.RetireSuperseded(ctx, vault, governedContext(), ledgerRef(), 1)
	if err != nil || outcome != RetireDone || len(vault.retired) != 1 || vault.retired[0] != 1 {
		t.Fatalf("vault retire = %q, %v, retired %v", outcome, err, vault.retired)
	}
	if states := ledgerStates(t, pool); states["retired"] != 1 || states["committed"] != 0 {
		t.Fatalf("states = %v", states)
	}
	// Vault: a version no ledger row owns (an operator wrote it) is still
	// destroyed: the path belongs to the reference.
	if outcome, err := ledger.RetireSuperseded(ctx, vault, governedContext(), ledgerRef(), 7); err != nil || outcome != RetireDone {
		t.Fatalf("unowned vault retire = %q, %v", outcome, err)
	}

	// pgvault: a name no committed row owns is kept.
	pgvault := &recordingStore{}
	name := SecretRef{Backend: BackendPGVault, Path: "legacy_name"}
	if outcome, err := ledger.RetireSuperseded(ctx, pgvault, governedContext(), name, 1); err != nil || outcome != RetireNotOwned ||
		len(pgvault.retired) != 0 {
		t.Fatalf("unowned pgvault retire = %q, %v, retired %v", outcome, err, pgvault.retired)
	}
	// pgvault: a name a governed write created is deleted.
	owned := SecretRef{Backend: BackendPGVault, Path: "governed_ref_abc"}
	if _, err := ledger.StagedWrite(ctx, pgvault, governedContext(), owned, value, 0); err != nil {
		t.Fatal(err)
	}
	if outcome, err := ledger.RetireSuperseded(ctx, pgvault, governedContext(), owned, 1); err != nil || outcome != RetireDone ||
		len(pgvault.retired) != 1 {
		t.Fatalf("owned pgvault retire = %q, %v, retired %v", outcome, err, pgvault.retired)
	}

	// A backend error leaves the row committed, so a retry still owns it.
	failing := &recordingStore{retireErr: Errorf(ErrUnavailable, OpRetire, "down")}
	again := SecretRef{Backend: BackendPGVault, Path: "governed_ref_def"}
	if _, err := ledger.StagedWrite(ctx, failing, governedContext(), again, value, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.RetireSuperseded(ctx, failing, governedContext(), again, 1); err == nil {
		t.Fatal("a backend error must be returned")
	}
	if states := ledgerStates(t, pool); states["committed"] != 1 || states["retired"] != 2 {
		t.Fatalf("states = %v", states)
	}
	// Not found counts as retired.
	failing.retireErr = Errorf(ErrNotFound, OpRetire, "gone")
	if outcome, err := ledger.RetireSuperseded(ctx, failing, governedContext(), again, 1); err != nil || outcome != RetireDone {
		t.Fatalf("not found retire = %q, %v", outcome, err)
	}

	// retired is final, and only a committed row can reach it.
	if _, err := pool.Exec(ctx, `UPDATE elitea_identity.secret_write_ledger SET state = 'committed' WHERE state = 'retired'`); err == nil {
		t.Fatal("a retired row must not change state")
	}
	if _, err := ledger.StagedWrite(ctx, &recordingStore{writeErr: Errorf(ErrUnavailable, OpWrite, "x")}, governedContext(),
		SecretRef{Backend: BackendVault, Mount: "secret", Path: "tenant7/other"}, value, 0); err == nil {
		t.Fatal("an unknown outcome must stay staged")
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_identity.secret_write_ledger SET state = 'retired' WHERE state = 'staged'`); err == nil {
		t.Fatal("a staged row must not move to retired")
	}
}
