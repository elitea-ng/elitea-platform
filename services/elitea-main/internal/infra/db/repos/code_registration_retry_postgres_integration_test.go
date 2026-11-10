package repos

// Real-PostgreSQL evidence for B3: the visit registrations of one run's Map
// items run together. Under SERIALIZABLE PostgreSQL aborts all but one with a
// serialization failure (40001); under the registration's READ COMMITTED
// transaction, whose first statement is the claim row lock, they commit one
// after another and leave no lock behind. Boundary crossed: real PostgreSQL,
// real migrations, the real claim/fence/lease lock
// (CodeIntentRepository.lockAccess) and the exact statements
// RegisterOriginalCodeVisit runs against original_code_visits; the full
// production path (permission check, source, policy) is
// code_registration_real_path_postgres_integration_test.go. Requires
// ELITEA_TEST_DATABASE_URL.

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

// Under SERIALIZABLE (the isolation before B3) the run's concurrent
// registrations fail: the B3 browser failure (40001 on the visit insert).
func TestOriginalCodeVisitRegistrationsConflictWithoutRetryPostgres(t *testing.T) {
	w, repo, claim := newRegistrationWorld(t, effectsParent, effectsClaim2, 3)
	errs := registerConcurrently(t, w, repo, claim, func(ctx context.Context, fn func(sqlExecutor) error) error {
		return repo.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, fn)
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

// Under the registration transaction every concurrent registration of the
// run commits in one attempt; none is answered unavailable or refused, and no
// lock is left.
func TestOriginalCodeVisitRegistrationsOfOneRunAllCommitPostgres(t *testing.T) {
	w, repo, claim := newRegistrationWorld(t, effectsForeign, effectsClaim3, 4)
	errs := registerConcurrently(t, w, repo, claim, func(ctx context.Context, fn func(sqlExecutor) error) error {
		return withinCodeRegistrationTx(ctx, repo.shared, fn)
	})
	for item, err := range errs {
		if err != nil {
			t.Fatalf("registration %d: %v (contended=%v)", item, err, errors.Is(err, code.ErrRegistrationContended))
		}
	}
	if got := countVisits(t, w, effectsForeign); got != registrationItems {
		t.Fatalf("visits = %d, want %d", got, registrationItems)
	}
	waitNoAdvisoryLocks(t, w)
}

// heldLocks counts the advisory locks, and the row-lock waits and open
// transactions of other sessions, in the test's own database.
func heldLocks(t *testing.T, w *effectsWorld) (advisory, waiting, open int) {
	t.Helper()
	if err := w.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())),
 (SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING(pid) WHERE NOT l.granted AND a.datname=current_database()),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND xact_start IS NOT NULL)`).Scan(&advisory, &waiting, &open); err != nil {
		t.Fatal(err)
	}
	return advisory, waiting, open
}

// waitNoAdvisoryLocks allows a cancelled session a moment to end its
// transaction, then requires no advisory lock, no lock wait and no open
// transaction to remain (row locks live only as long as their transaction).
func waitNoAdvisoryLocks(t *testing.T, w *effectsWorld) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		advisory, waiting, open := heldLocks(t, w)
		if advisory == 0 && waiting == 0 && open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("left behind: %d advisory locks, %d lock waits, %d open transactions", advisory, waiting, open)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireRunFree proves nothing holds the run: a registration of the same
// run commits within a bound it could not meet if it had to wait.
func requireRunFree(t *testing.T, repo *CodeIntentRepository, claim storage.ContentClaim, item int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := withinCodeRegistrationTx(ctx, repo.shared, func(tx sqlExecutor) error {
		return registerVisitStatements(ctx, repo, tx, claim, item, func() {})
	}); err != nil {
		t.Fatalf("the next registration of the run waited or failed: %v", err)
	}
}

// A registration whose ctx is cancelled while it waits on the run's claim
// lock ends; when the holder commits nothing of the cancelled one remains.
func TestRunRegistrationCancelledWhileWaitingLeavesNoLockPostgres(t *testing.T) {
	w, repo, claim := newRegistrationWorld(t, effectsParent, effectsClaim2, 3)
	holding, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- withinCodeRegistrationTx(t.Context(), repo.shared, func(tx sqlExecutor) error {
			return registerVisitStatements(t.Context(), repo, tx, claim, 0, func() {
				close(holding)
				<-release
			})
		})
	}()
	<-holding
	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan error, 1)
	go func() {
		waited <- withinCodeRegistrationTx(ctx, repo.shared, func(tx sqlExecutor) error {
			return registerVisitStatements(ctx, repo, tx, claim, 1, func() {})
		})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, waiting, _ := heldLocks(t, w); waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second registration never waited on the claim lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-waited; err == nil {
		t.Fatal("a cancelled registration committed")
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatalf("the holder failed: %v", err)
	}
	waitNoAdvisoryLocks(t, w)
	requireRunFree(t, repo, claim, 2)
	if got := countVisits(t, w, effectsParent); got != 2 {
		t.Fatalf("visits = %d, want the holder's and the next one", got)
	}
}

// A killed Worker: 4 registrations in flight, one holding the run's claim
// lock and three waiting, then every ctx is cancelled. Nothing is left
// locked and the next registration of the run commits at once.
func TestRunRegistrationsCancelledInFlightLeaveNoLockPostgres(t *testing.T) {
	w, repo, claim := newRegistrationWorld(t, effectsParent, effectsClaim2, 3)
	ctx, cancel := context.WithCancel(t.Context())
	holding := make(chan struct{})
	errs := make(chan error, 4)
	go func() {
		errs <- withinCodeRegistrationTx(ctx, repo.shared, func(tx sqlExecutor) error {
			return registerVisitStatements(ctx, repo, tx, claim, 0, func() {
				close(holding)
				<-ctx.Done()
			})
		})
	}()
	<-holding
	for item := 1; item < 4; item++ {
		go func() {
			errs <- withinCodeRegistrationTx(ctx, repo.shared, func(tx sqlExecutor) error {
				return registerVisitStatements(ctx, repo, tx, claim, item, func() {})
			})
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, waiting, _ := heldLocks(t, w); waiting >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the three registrations never waited on the claim lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	for range 4 {
		if err := <-errs; err == nil {
			t.Fatal("a cancelled registration committed")
		}
	}
	waitNoAdvisoryLocks(t, w)
	requireRunFree(t, repo, claim, 4)
	if got := countVisits(t, w, effectsParent); got != 1 {
		t.Fatalf("visits = %d, want only the one registered after the kill", got)
	}
}
