package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestPostgresAdmissionCapAbove1024IsPersistedAndChangedInOneStep is the #966
// gate: a cap above the former 1024 ceiling is accepted by shared/0170 and
// enforced, and SetAdmissionPolicyMax — the operator command's whole effect —
// moves the persisted cap so replicas configured with the new value admit
// while a replica still on the old value fails closed with the fix in its
// error.
func TestPostgresAdmissionCapAbove1024IsPersistedAndChangedInOneStep(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, constraint := range []struct{ table, name string }{
		{"execution_admission_policies", "execution_admission_policy_max_outstanding"},
		{"agent_admission_reservations", "agent_admission_reservation_configured_max"},
	} {
		var definition string
		if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint AS c
JOIN pg_class AS t ON t.oid = c.conrelid
JOIN pg_namespace AS n ON n.oid = t.relnamespace
WHERE n.nspname = 'elitea_runtime' AND t.relname = $1 AND c.conname = $2`,
			constraint.table, constraint.name).Scan(&definition); err != nil {
			t.Fatalf("load %s.%s: %v", constraint.table, constraint.name, err)
		}
		if !strings.Contains(definition, "4096") {
			t.Fatalf("%s.%s is not the 4096 ceiling: %s", constraint.table, constraint.name, definition)
		}
	}

	oldPolicy := testDispatchPolicy()
	oldPolicy.MaxOutstanding = 2048
	oldRepository, err := NewExecutionJobsRepository(pool, oldPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldRepository.AdmitValidation(ctx, postgresCapacityAdmission(0)); err != nil {
		t.Fatalf("admission under a 2048 cap: %v", err)
	}
	policies, err := ListAdmissionPolicies(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || policies[0].CapabilityID != "configuration.validate.v1" || policies[0].MaxOutstanding != 2048 {
		t.Fatalf("2048 cap was not persisted: %+v", policies)
	}

	written, err := SetAdmissionPolicyMax(ctx, pool, "", MaxSupportedOutstandingJobs)
	if err != nil || written != 1 {
		t.Fatalf("set every persisted cap: written=%d err=%v", written, err)
	}
	_, err = oldRepository.AdmitValidation(ctx, postgresCapacityAdmission(1))
	if !errors.Is(err, ErrAdmissionPolicyMismatch) {
		t.Fatalf("replica on the old cap did not fail closed: %v", err)
	}
	if want := fmt.Sprintf("configured=2048 persisted=%d", MaxSupportedOutstandingJobs); !strings.Contains(err.Error(), want) {
		t.Fatalf("mismatch message lacks %q: %v", want, err)
	}

	newPolicy := oldPolicy
	newPolicy.MaxOutstanding = MaxSupportedOutstandingJobs
	newRepository, err := NewExecutionJobsRepository(pool, newPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRepository.AdmitValidation(ctx, postgresCapacityAdmission(1)); err != nil {
		t.Fatalf("replica on the new cap: %v", err)
	}

	if written, err := SetAdmissionPolicyMax(ctx, pool, "agent.execute.v1", 3000); err != nil || written != 1 {
		t.Fatalf("set one named capability: written=%d err=%v", written, err)
	}
	if _, err := SetAdmissionPolicyMax(ctx, pool, "", MaxSupportedOutstandingJobs+1); err == nil {
		t.Fatal("accepted a cap above the supported ceiling")
	}
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.execution_admission_policies SET max_outstanding = 4097`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("the CHECK accepted 4097: %v", err)
	}
}
