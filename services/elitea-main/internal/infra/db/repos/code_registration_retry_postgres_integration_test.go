package repos

// Real-PostgreSQL evidence for B3: the visit registrations of one run's Map
// items run together, and PostgreSQL aborts all but one with a serialization
// failure (40001). Boundary crossed: real PostgreSQL, real migrations, the real
// claim/fence/lease lock (CodeIntentRepository.lockAccess) and the exact
// statements RegisterOriginalCodeVisit runs against original_code_visits. The
// saved-source and YAML policy decode before them is not rebuilt here (it reads
// and writes no shared row). Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	recoveryapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/noderecovery"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

const registrationItems = 8

// registerVisitStatements are RegisterOriginalCodeVisit's statements after
// the policy decode: the claim lock, the per-run visit count, the insert and
// the locked read-back.
func registerVisitStatements(ctx context.Context, repo *CodeIntentRepository, tx sqlExecutor, claim storage.ContentClaim, item int, inserted func()) error {
	if _, err := repo.lockAccess(ctx, tx, claim, "RUNNING"); err != nil {
		return err
	}
	visit, activation := fmt.Sprintf("%064x", 0xa000+item), fmt.Sprintf("%064x", 0xb000+item)
	var others int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.original_code_visits WHERE execution_id=$1 AND generation=$2 AND visit_id<>$3`, claim.ExecutionID, int64(claim.Generation), visit).Scan(&others); err != nil || others >= 4096 {
		return code.ErrRejected
	}
	if _, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.original_code_visits(execution_id,generation,visit_id,visit_digest,activation_id,attempt,record_json,registered_claim_id) VALUES($1,$2,$3,$4,$5,1,'{}'::bytea,$6) ON CONFLICT DO NOTHING`, claim.ExecutionID, int64(claim.Generation), visit, effectsHex("c"), activation, claim.ClaimID); err != nil {
		return err
	}
	inserted()
	var saved []byte
	if err := tx.QueryRow(ctx, `SELECT record_json FROM elitea_runtime.original_code_visits WHERE execution_id=$1 AND generation=$2 AND visit_id=$3 FOR UPDATE`, claim.ExecutionID, int64(claim.Generation), visit).Scan(&saved); err != nil {
		return code.ErrRejected
	}
	return nil
}

// registerConcurrently runs registrationItems registrations of one run. The
// first holds its transaction open after its insert until every other one is
// waiting on the claim lock with its snapshot already taken, so the conflict
// PostgreSQL reports is certain rather than timing-dependent.
func registerConcurrently(t *testing.T, w *effectsWorld, repo *CodeIntentRepository, claim storage.ContentClaim, run func(context.Context, func(sqlExecutor) error) error) []error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	firstInserted, releaseFirst := make(chan struct{}), make(chan struct{})
	var once sync.Once
	errs := make([]error, registrationItems)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = run(ctx, func(tx sqlExecutor) error {
			return registerVisitStatements(ctx, repo, tx, claim, 0, func() {
				once.Do(func() {
					close(firstInserted)
					<-releaseFirst
				})
			})
		})
	}()
	<-firstInserted
	for item := 1; item < registrationItems; item++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[item] = run(ctx, func(tx sqlExecutor) error {
				return registerVisitStatements(ctx, repo, tx, claim, item, func() {})
			})
		}()
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= registrationItems-1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d registrations waited on the claim lock", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(releaseFirst)
	wg.Wait()
	return errs
}

func newRegistrationWorld(t *testing.T, execution, claimID string, fence byte) (*effectsWorld, *CodeIntentRepository, storage.ContentClaim) {
	t.Helper()
	w := newEffectsWorld(t)
	repo := &CodeIntentRepository{permissions: func(context.Context, sqlExecutor, recoveryapp.Selector, string) error { return nil }}
	var err error
	if repo.shared, err = newPostgresSharedStore(w.pool); err != nil {
		t.Fatal(err)
	}
	project := w.project()
	w.job(execution, project, "RUNNING")
	w.admit(execution)
	return w, repo, w.access(execution, project, claimID, 1, fence)
}

func countVisits(t *testing.T, w *effectsWorld, execution string) int {
	t.Helper()
	var visits int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM elitea_runtime.original_code_visits WHERE execution_id=$1`, execution).Scan(&visits); err != nil {
		t.Fatal(err)
	}
	return visits
}

// Without the bounded retry the run's concurrent registrations fail: this is
// the B3 browser failure (40001 on the visit insert, answered Unavailable, or
// on the claim lock, answered as a refusal).
func TestOriginalCodeVisitRegistrationsConflictWithoutRetryPostgres(t *testing.T) {
	w, repo, claim := newRegistrationWorld(t, effectsParent, effectsClaim2, 3)
	errs := registerConcurrently(t, w, repo, claim, func(ctx context.Context, fn func(sqlExecutor) error) error {
		return repo.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
			conflict := false
			err := fn(conflictRecordingExecutor{next: tx, conflict: &conflict})
			if err != nil && conflict {
				return fmt.Errorf("%w (a statement met a conflict)", err)
			}
			return err
		})
	})
	if errs[0] != nil {
		t.Fatalf("the first registration failed: %v", errs[0])
	}
	failed := 0
	for _, err := range errs[1:] {
		if err != nil {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("no concurrent registration conflicted; the fixture no longer reproduces B3")
	}
	if got := countVisits(t, w, effectsParent); got != registrationItems-failed {
		t.Fatalf("visits = %d, want %d", got, registrationItems-failed)
	}
}

// With it, every concurrent registration of the run commits; none is
// answered unavailable or refused.
func TestOriginalCodeVisitRegistrationsOfOneRunAllCommitPostgres(t *testing.T) {
	w, repo, claim := newRegistrationWorld(t, effectsForeign, effectsClaim3, 4)
	errs := registerConcurrently(t, w, repo, claim, func(ctx context.Context, fn func(sqlExecutor) error) error {
		return withinCodeRegistrationTx(ctx, repo.shared, claim.ExecutionID, claim.Generation, fn)
	})
	for item, err := range errs {
		if err != nil {
			t.Fatalf("registration %d: %v (contended=%v)", item, err, errors.Is(err, code.ErrRegistrationContended))
		}
	}
	if got := countVisits(t, w, effectsForeign); got != registrationItems {
		t.Fatalf("visits = %d, want %d", got, registrationItems)
	}
}
