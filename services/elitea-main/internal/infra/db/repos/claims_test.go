package repos

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestClaimCapabilityGateIncludesLanguageNeutralAgentWorkers(t *testing.T) {
	for _, capabilityID := range []string{
		executiondomain.ConfigurationValidationCapability,
		executiondomain.IndexIngestCapability,
		executiondomain.AgentApplicationCapability,
		executiondomain.AgentAdhocCapability,
	} {
		if !claimCapabilityAllowed(capabilityID) {
			t.Fatalf("claim capability %q was rejected", capabilityID)
		}
	}
	if claimCapabilityAllowed("index.search.v1") {
		t.Fatal("standalone search capability was accepted by the claim gate")
	}
}

func TestClaimValidationRequiresExactPublishedEnvelopeBeforeLease(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	otherDigest := runtimedomain.SHA256([]byte("other-signed-envelope"))
	tests := []struct {
		name             string
		jobState         string
		published        bool
		preparedDigest   []byte
		publishedDigest  []byte
		requestDigest    runtimedomain.Digest
		wantErr          error
		wantRetryNoLease bool
	}{
		{
			name:             "append succeeded but outbox mark is not durable",
			jobState:         "PENDING",
			published:        false,
			requestDigest:    publishedDigest,
			wantRetryNoLease: true,
		},
		{
			name:            "published envelope digest mismatch",
			jobState:        "DISPATCHED",
			published:       true,
			preparedDigest:  publishedDigest[:],
			publishedDigest: publishedDigest[:],
			requestDigest:   otherDigest,
			wantErr:         executionapp.ErrInvalidClaim,
		},
		{
			name:            "published digest differs from prepared winner",
			jobState:        "DISPATCHED",
			published:       true,
			preparedDigest:  publishedDigest[:],
			publishedDigest: otherDigest[:],
			requestDigest:   publishedDigest,
			wantErr:         executionapp.ErrInvalidClaim,
		},
		{
			name:             "durably quarantined input",
			jobState:         "QUARANTINED",
			published:        true,
			preparedDigest:   publishedDigest[:],
			publishedDigest:  publishedDigest[:],
			requestDigest:    publishedDigest,
			wantRetryNoLease: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
				claimExecutionRow("RUNNING", test.jobState, test.published, test.preparedDigest, test.publishedDigest, false),
			}}}
			generatorCalled := false
			repository, err := newClaimsRepository(
				store,
				func() (string, error) {
					generatorCalled = true
					return "claim-1", nil
				},
				func() (runtimedomain.FenceToken, error) {
					generatorCalled = true
					return runtimedomain.FenceToken(runtimedomain.SHA256([]byte("token"))), nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(test.requestDigest), executionapp.MaxClaimLeaseTTLMillis)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("unexpected claim error: got %v want %v", err, test.wantErr)
			}
			if test.wantErr != nil && decision.Lease != (runtimedomain.ActiveLease{}) {
				t.Fatalf("rejected command received a lease: %+v", decision.Lease)
			}
			if test.wantRetryNoLease && (decision.Disposition != executionapp.ClaimRetryLaterNoACK || decision.Lease != (runtimedomain.ActiveLease{}) || decision.DesiredState != runtimedomain.DesiredRunning) {
				t.Fatalf("unsafe no-lease retry decision: %+v", decision)
			}
			if generatorCalled || len(store.rowCalls) != 1 || len(store.execCalls) != 0 {
				t.Fatalf("unbound/quarantined command allocated or mutated a claim: generated=%t rows=%d execs=%d", generatorCalled, len(store.rowCalls), len(store.execCalls))
			}
		})
	}
}

func TestClaimValidationScopesDurableLookupToRequestedCapability(t *testing.T) {
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		claimExecutionRow("RUNNING", "PENDING", false, nil, nil, false),
	}}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "must-not-generate", nil },
		func() (runtimedomain.FenceToken, error) {
			return runtimedomain.FenceToken(runtimedomain.SHA256([]byte("must-not-generate"))), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(runtimedomain.SHA256([]byte("index-envelope")))
	request.CapabilityID = executiondomain.IndexIngestCapability
	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil || decision.Disposition != executionapp.ClaimRetryLaterNoACK {
		t.Fatalf("index capability lookup decision=%+v err=%v", decision, err)
	}
	if len(store.rowCalls) != 1 || len(store.rowCalls[0].args) != 3 || store.rowCalls[0].args[2] != executiondomain.IndexIngestCapability {
		t.Fatalf("claim lookup escaped requested capability: %#v", store.rowCalls)
	}

	request.CapabilityID = "unknown.capability.v1"
	if _, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis); !errors.Is(err, executionapp.ErrInvalidClaim) {
		t.Fatalf("unknown capability error=%v", err)
	}
	if len(store.rowCalls) != 1 {
		t.Fatal("unknown capability reached PostgreSQL")
	}
}

func TestRetiredPreparedDeliveryReturnsTypedFenceFreeACK(t *testing.T) {
	digest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	tests := []struct {
		name             string
		desiredState     string
		jobState         string
		terminalCode     string
		published        bool
		retirementCode   string
		wantDisposition  executionapp.ClaimDisposition
		wantDesiredState runtimedomain.DesiredState
		wantReason       executionapp.RetirementReason
	}{
		{
			name:             "deadline leaked before publication mark",
			desiredState:     "RUNNING",
			jobState:         "FAILED",
			terminalCode:     retirementCodeDeadlineExceeded,
			retirementCode:   retirementCodeDeadlineExceeded,
			wantDisposition:  executionapp.ClaimRetiredACK,
			wantDesiredState: runtimedomain.DesiredRunning,
			wantReason:       executionapp.RetirementDeadlineExceeded,
		},
		{
			name:             "deadline published but never claimed",
			desiredState:     "RUNNING",
			jobState:         "FAILED",
			terminalCode:     retirementCodeDeadlineExceeded,
			published:        true,
			retirementCode:   retirementCodeDeadlineExceeded,
			wantDisposition:  executionapp.ClaimRetiredACK,
			wantDesiredState: runtimedomain.DesiredRunning,
			wantReason:       executionapp.RetirementDeadlineExceeded,
		},
		{
			name:             "cancellation leaked before publication mark",
			desiredState:     "CANCELLED",
			jobState:         "CANCELLED",
			retirementCode:   retirementCodeCancelled,
			wantDisposition:  executionapp.ClaimObsoleteACK,
			wantDesiredState: runtimedomain.DesiredCancelled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publishedDigest := []byte(nil)
			if test.published {
				publishedDigest = digest[:]
			}
			store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
				"command-1",
				test.desiredState,
				test.jobState,
				"NOT_STARTED",
				test.terminalCode,
				"outbox-1",
				test.published,
				digest[:],
				publishedDigest,
				true,
				test.retirementCode,
				true,
				false,
				int64(1),
			}}}}}
			generated := false
			repository, err := newClaimsRepository(
				store,
				func() (string, error) { generated = true; return "unsafe", nil },
				func() (runtimedomain.FenceToken, error) { generated = true; return runtimedomain.FenceToken{}, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(digest), executionapp.MaxClaimLeaseTTLMillis)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Disposition != test.wantDisposition || decision.DesiredState != test.wantDesiredState || decision.RetirementReason != test.wantReason || decision.Lease != (runtimedomain.ActiveLease{}) {
				t.Fatalf("unexpected retired delivery decision: %+v", decision)
			}
			if generated || len(store.rowCalls) != 1 || len(store.execCalls) != 0 {
				t.Fatalf("retired delivery allocated or mutated authority: generated=%t rows=%d execs=%d", generated, len(store.rowCalls), len(store.execCalls))
			}
		})
	}
}

func TestPublishedBeforeDeadlineClaimedAfterDeadlineRetiresWithoutAuthority(t *testing.T) {
	digest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: []any{
				"command-1", "RUNNING", "DISPATCHED", "NOT_STARTED", "", "outbox-1", true,
				digest[:], digest[:], false, "", true, false, int64(1),
			}},
			{values: []any{int64(1)}},
		},
	}}
	generated := false
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { generated = true; return "unsafe", nil },
		func() (runtimedomain.FenceToken, error) { generated = true; return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(digest), executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRetiredACK || decision.RetirementReason != executionapp.RetirementDeadlineExceeded || decision.Lease != (runtimedomain.ActiveLease{}) {
		t.Fatalf("late claim received authority: %+v", decision)
	}
	if generated || len(store.rowCalls) != 2 || len(store.execCalls) != 1 || !strings.Contains(store.execCalls[0].sql, "execution_replay_events") {
		t.Fatalf("claim-time retirement did not remain authority-free and auditable: generated=%t rows=%d execs=%d", generated, len(store.rowCalls), len(store.execCalls))
	}
}

func TestClaimValidationPreservesDurableLookupFailure(t *testing.T) {
	databaseFailure := errors.New("database temporarily unavailable")
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{err: databaseFailure}}}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ClaimValidation(
		context.Background(),
		testClaimRequest(runtimedomain.SHA256([]byte("published-signed-envelope"))),
		executionapp.MaxClaimLeaseTTLMillis,
	)
	if !errors.Is(err, databaseFailure) || !errors.Is(err, executionapp.ErrClaimDependencyUnavailable) || errors.Is(err, executionapp.ErrInvalidClaim) {
		t.Fatalf("durable lookup failure was masked as a protocol error: %v", err)
	}
}

func TestClaimValidationAllocatesMonotonicAttemptAndLeaseEpoch(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("first-token")))
	leaseExpires := time.Date(2030, time.July, 16, 13, 0, 0, 0, time.UTC)
	leaseObservedAt := leaseExpires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration())
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "DISPATCHED", true, publishedDigest[:], publishedDigest[:], false),
			{err: pgx.ErrNoRows},
			{values: []any{int64(1)}},
			{values: []any{leaseExpires, leaseObservedAt, int64(0)}},
			{err: pgx.ErrNoRows},
			{values: []any{false}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "claim-1", nil },
		func() (runtimedomain.FenceToken, error) { return token, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimAccepted || decision.Lease.Fence.ClaimAttempt != 1 || decision.Lease.Fence.LeaseEpoch != 1 || decision.LeaseObservedAt != leaseObservedAt || decision.ClaimHandoffWatermark != 0 {
		t.Fatalf("first claim did not allocate matching monotonic authority: %+v", decision)
	}
	if !strings.Contains(store.rowCalls[3].sql, "$7, $7, $8,") || !strings.Contains(store.rowCalls[3].sql, "clock_timestamp() AS observed_at") || !strings.Contains(store.rowCalls[3].sql, "$9::bigint * interval '1 millisecond'") || !strings.Contains(store.rowCalls[3].sql, "initial_output_watermark") || !strings.Contains(store.rowCalls[3].sql, "execution_replay_state") || !strings.Contains(store.rowCalls[3].sql, "last_node_sequence") || !strings.Contains(store.execCalls[0].sql, "authority_granted_at = clock_timestamp()") {
		t.Fatal("claim insert does not derive lease_epoch from the monotonic claim attempt")
	}
}

func TestIndexClaimPersistsDurableNodeEventHandoffWatermark(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-index-envelope"))
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("index-handoff-token")))
	leaseExpires := time.Date(2030, time.July, 22, 13, 0, 0, 0, time.UTC)
	leaseObservedAt := leaseExpires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration())
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "DISPATCHED", true, publishedDigest[:], publishedDigest[:], false),
			{err: pgx.ErrNoRows},
			{values: []any{int64(2)}},
			{values: []any{leaseExpires, leaseObservedAt, int64(3)}},
			{err: pgx.ErrNoRows},
			{values: []any{false}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "claim-index-2", nil },
		func() (runtimedomain.FenceToken, error) { return token, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(publishedDigest)
	request.CapabilityID = executiondomain.IndexIngestCapability
	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimAccepted || decision.ClaimHandoffWatermark != 3 || decision.Lease.Fence.ClaimAttempt != 2 {
		t.Fatalf("index claim lost its durable output handoff: %+v", decision)
	}
}

func TestRenewLeasePreservesEpochAndFullFence(t *testing.T) {
	fence := testValidationFrame(t).Fence
	fence.ClaimAttempt = 2
	fence.LeaseEpoch = 2
	expires := time.Date(2030, time.July, 16, 14, 0, 0, 0, time.UTC)
	observedAt := expires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration())
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"claim-2",
		fence.CommandID,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.WorkloadIdentity,
		fence.WorkloadSessionID,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
		expires,
		"RUNNING",
		true,
		observedAt,
	}}}}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, gotObservedAt, err := repository.RenewLease(context.Background(), fence, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Fence != fence || lease.Fence.LeaseEpoch != 2 || gotObservedAt != observedAt || lease.ExpiresAt.Sub(gotObservedAt) != executionapp.MaxClaimLeaseTTLMillis.Duration() {
		t.Fatalf("renewal changed the authority-bearing fence: %+v", lease.Fence)
	}
	sql := store.rowCalls[0].sql
	if strings.Contains(sql, "lease_epoch = c.lease_epoch + 1") || !strings.Contains(sql, "SET lease_expires_at = authority_clock.observed_at + ($10::bigint * interval '1 millisecond')") || !strings.Contains(sql, "c.lease_epoch = $8") {
		t.Fatal("renewal must extend time while preserving and verifying the exact epoch")
	}
}

func TestActiveClaimHidesOwnerFenceFromDifferentWorkload(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("owner-token")))
	expires := time.Date(2030, time.July, 16, 13, 0, 0, 0, time.UTC)
	observedAt := expires.Add(-time.Second)
	ownerLeaseRow := func(identity, session, producer string) scriptedRow {
		return scriptedRow{values: []any{
			"owner-claim", "command-1", "execution-1", int64(1),
			identity, session, producer, int64(1), int64(1), token[:],
			expires, "RUNNING", true, observedAt,
		}}
	}

	t.Run("different authenticated claimant gets no lease", func(t *testing.T) {
		store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "CLAIMED", true, publishedDigest[:], publishedDigest[:], true),
			ownerLeaseRow("spiffe://elitea.test/workload/owner", "owner-session", "owner-producer"),
		}}}
		repository, err := newClaimsRepository(store, func() (string, error) { return "unused", nil }, func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Disposition != executionapp.ClaimRetryLaterNoACK || decision.Lease != (runtimedomain.ActiveLease{}) || decision.DesiredState != runtimedomain.DesiredRunning {
			t.Fatalf("different claimant received owner authority: %+v", decision)
		}
		if len(store.rowCalls) != 2 || len(store.execCalls) != 0 {
			t.Fatalf("different claimant mutated/recovered owner claim: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
		}
	})

	t.Run("same workload session keeps active noack recovery", func(t *testing.T) {
		request := testClaimRequest(publishedDigest)
		store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "CLAIMED", true, publishedDigest[:], publishedDigest[:], true),
			ownerLeaseRow(request.WorkloadIdentity, request.WorkloadSessionID, request.ProducerID),
			{err: pgx.ErrNoRows},
		}}}
		repository, err := newClaimsRepository(store, func() (string, error) { return "unused", nil }, func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Disposition != executionapp.ClaimActiveLeaseNoACK || decision.Lease.Fence.WorkloadSessionID != request.WorkloadSessionID || decision.Lease.Fence.Token != token {
			t.Fatalf("same owner lost active-lease recovery: %+v", decision)
		}
	})
}

func TestNeverStartedCancellationFinalizesWithoutLease(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	for _, state := range []string{"PENDING", "DISPATCHED"} {
		t.Run(state, func(t *testing.T) {
			generatorCalled := false
			store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
				rowResults: []scriptedRow{
					claimExecutionRow("CANCELLED", state, true, publishedDigest[:], publishedDigest[:], false),
					{values: []any{int64(1)}},
				},
			}}
			repository, err := newClaimsRepository(
				store,
				func() (string, error) {
					generatorCalled = true
					return "unsafe-claim", nil
				},
				func() (runtimedomain.FenceToken, error) {
					generatorCalled = true
					return runtimedomain.FenceToken(runtimedomain.SHA256([]byte("unsafe-token"))), nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Disposition != executionapp.ClaimObsoleteACK || decision.Lease != (runtimedomain.ActiveLease{}) || decision.DesiredState != runtimedomain.DesiredCancelled {
				t.Fatalf("never-started cancellation was not finalized before ACK: %+v", decision)
			}
			if generatorCalled || len(store.rowCalls) != 2 || len(store.execCalls) != 1 {
				t.Fatalf("cancellation allocated authority or skipped its one terminal mutation: generated=%t rows=%d execs=%d", generatorCalled, len(store.rowCalls), len(store.execCalls))
			}
			for _, predicate := range []string{
				"retirement_code = $4",
				"j.desired_state = 'CANCELLED'",
				"j.state IN ('PENDING', 'DISPATCHED')",
				"NOT EXISTS",
				"FROM elitea_runtime.execution_claims AS c",
			} {
				if !strings.Contains(store.rowCalls[1].sql, predicate) {
					t.Fatalf("never-started cancellation SQL is missing %q", predicate)
				}
			}
			if !strings.Contains(store.execCalls[0].sql, "execution_replay_events") {
				t.Fatal("never-started cancellation omitted its auditable replay event")
			}
		})
	}
}

func TestAlreadyCancelledClaimReturnsObsoleteWithoutLease(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		claimExecutionRow("CANCELLED", "CANCELLED", true, publishedDigest[:], publishedDigest[:], false),
	}}}
	repository, err := newClaimsRepository(store, func() (string, error) { return "unused", nil }, func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimObsoleteACK || decision.Lease != (runtimedomain.ActiveLease{}) || decision.DesiredState != runtimedomain.DesiredCancelled {
		t.Fatalf("durably cancelled command was not ACK-safe: %+v", decision)
	}
	if len(store.rowCalls) != 1 || len(store.execCalls) != 0 {
		t.Fatalf("already terminal cancellation performed unnecessary claim work: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
	}
}

func TestAmbiguousCancellationAndDrainingRemainNoLeaseRetries(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	tests := []struct {
		name    string
		desired runtimedomain.DesiredState
		state   string
		rows    int
	}{
		{name: "claimed without live owner", desired: runtimedomain.DesiredCancelled, state: "CLAIMED", rows: 3},
		{name: "running without live owner", desired: runtimedomain.DesiredCancelled, state: "RUNNING", rows: 3},
		{name: "settling without live owner", desired: runtimedomain.DesiredCancelled, state: "SETTLING", rows: 3},
		{name: "draining is not terminal", desired: runtimedomain.DesiredDraining, state: "DISPATCHED", rows: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generatorCalled := false
			rows := []scriptedRow{
				claimExecutionRow(string(test.desired), test.state, true, publishedDigest[:], publishedDigest[:], test.desired == runtimedomain.DesiredCancelled),
				{err: pgx.ErrNoRows},
			}
			if test.desired == runtimedomain.DesiredCancelled {
				rows = append(rows, scriptedRow{values: []any{false}})
			}
			store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: rows}}
			repository, err := newClaimsRepository(
				store,
				func() (string, error) { generatorCalled = true; return "unsafe-claim", nil },
				func() (runtimedomain.FenceToken, error) {
					generatorCalled = true
					return runtimedomain.FenceToken(runtimedomain.SHA256([]byte("unsafe-token"))), nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Disposition != executionapp.ClaimRetryLaterNoACK || decision.Lease != (runtimedomain.ActiveLease{}) || decision.DesiredState != test.desired {
				t.Fatalf("ambiguous non-running command received authority or ACK: %+v", decision)
			}
			if generatorCalled || len(store.rowCalls) != test.rows || len(store.execCalls) != 0 {
				t.Fatalf("ambiguous non-running command mutated authority: generated=%t rows=%d execs=%d", generatorCalled, len(store.rowCalls), len(store.execCalls))
			}
		})
	}
}

func TestExpiredCancelledClaimRecoversDurableCanonicalCancellationAcrossPod(t *testing.T) {
	frame := testValidationFrame(t)
	source, _, err := validationOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, _, err := canonicalCancellationOutput(source)
	if err != nil {
		t.Fatal(err)
	}
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	newToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("cancel-recovery-token")))
	leaseExpires := time.Date(2030, time.July, 17, 13, 0, 0, 0, time.UTC)
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("CANCELLED", "CLAIMED", true, publishedDigest[:], publishedDigest[:], true),
			{values: []any{
				"old-claim", "command-1", "execution-1", int64(1),
				frame.Fence.WorkloadIdentity, frame.Fence.WorkloadSessionID, frame.Fence.ProducerID,
				int64(1), int64(1), frame.Fence.Token[:],
				time.Date(2026, time.July, 17, 11, 0, 0, 0, time.UTC), "CANCELLED", false,
				time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC),
			}},
			{values: []any{true}},
			{values: []any{int64(2)}},
			{values: []any{leaseExpires, leaseExpires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration()), int64(0)}},
			{values: []any{
				cancelled.SettlementProposalID,
				string(cancelled.SettlementOutcome),
				cancelled.LogicalOutputID,
				cancelled.EventID,
				int64(cancelled.Sequence),
				cancelled.PayloadDigest[:],
				cancelled.SettlementBytes,
				cancelled.SettlementDigest[:],
				cancelled.SettlementKey,
				int64(cancelled.ClaimHandoffWatermark),
			}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1")},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "cancel-recovery-claim", nil },
		func() (runtimedomain.FenceToken, error) { return newToken, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(publishedDigest)
	request.WorkloadIdentity = "spiffe://elitea.test/workload/replacement"
	request.WorkloadSessionID = "replacement-session"
	request.ProducerID = "replacement-producer"

	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRecoverTerminalACK || decision.SettlementRecovery == nil || decision.SettlementRecovery.Proposal == nil {
		t.Fatalf("durable cancellation was not recovered after lease expiry: %+v", decision)
	}
	if decision.Lease.DesiredState != runtimedomain.DesiredCancelled || decision.Lease.Fence.WorkloadIdentity != request.WorkloadIdentity || decision.Lease.Fence.ClaimAttempt != 2 {
		t.Fatalf("cancellation recovery authority was not narrowly rebound: %+v", decision.Lease)
	}
	proposal := decision.SettlementRecovery.Proposal
	if proposal.Outcome != executionapp.SettlementCancelled || proposal.Fence != decision.Lease.Fence || proposal.TerminalPayloadDigest != cancelled.PayloadDigest {
		t.Fatalf("recovered cancellation proposal lost its terminal binding: %+v", proposal)
	}
	if len(store.rowCalls) != 6 || !strings.Contains(store.rowCalls[2].sql, "projected_at IS NOT NULL") || !strings.Contains(store.rowCalls[5].sql, "source_claim.release_reason = 'LEASE_EXPIRED'") {
		t.Fatal("cancelled replacement authority was not gated by a durable predecessor output")
	}
}

func TestCancelledRecoveryAuthorityFailsClosedWhenExactTerminalLoadMisses(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("recovery-only-token")))
	leaseExpires := time.Date(2030, time.July, 17, 14, 0, 0, 0, time.UTC)
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("CANCELLED", "CLAIMED", true, publishedDigest[:], publishedDigest[:], true),
			{err: pgx.ErrNoRows},
			{values: []any{true}},
			{values: []any{int64(2)}},
			{values: []any{leaseExpires, leaseExpires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration()), int64(0)}},
			{err: pgx.ErrNoRows},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "must-rollback-claim", nil },
		func() (runtimedomain.FenceToken, error) { return token, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
	if !errors.Is(err, executionapp.ErrClaimDependencyUnavailable) {
		t.Fatalf("missing exact recovery output did not fail the transaction: %v", err)
	}
	if decision != (executionapp.ClaimDecision{}) {
		t.Fatalf("recovery-only authority escaped as an executable decision: %+v", decision)
	}
	if len(store.rowCalls) != 6 || len(store.execCalls) != 1 {
		t.Fatalf("recovery-only path did not stop at the exact terminal miss: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
	}
}

func TestCancellationFinalizerFailsClosedWhenAtomicTransitionDoesNotMatch(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("CANCELLED", "DISPATCHED", true, publishedDigest[:], publishedDigest[:], false),
			{err: pgx.ErrNoRows},
		},
	}}
	repository, err := newClaimsRepository(store, func() (string, error) { return "unused", nil }, func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRetryLaterNoACK || decision.DesiredState != runtimedomain.DesiredCancelled || decision.Lease != (runtimedomain.ActiveLease{}) {
		t.Fatalf("unmatched terminal transition became ACK-safe: %+v", decision)
	}
}

func TestExpiredPriorAuthorityIsReleasedButNotMisclassifiedAsNeverStartedCancellation(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("expired-token")))
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("CANCELLED", "DISPATCHED", true, publishedDigest[:], publishedDigest[:], true),
			{values: []any{
				"expired-claim", "command-1", "execution-1", int64(1),
				"spiffe://elitea.test/workload/old", "old-session", "old-producer",
				int64(1), int64(1), token[:], time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC),
				"CANCELLED", false, time.Date(2026, time.July, 17, 11, 0, 0, 0, time.UTC),
			}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
	}}
	generatorCalled := false
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { generatorCalled = true; return "unsafe-claim", nil },
		func() (runtimedomain.FenceToken, error) { generatorCalled = true; return token, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), testClaimRequest(publishedDigest), executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRetryLaterNoACK || decision.DesiredState != runtimedomain.DesiredCancelled || decision.Lease != (runtimedomain.ActiveLease{}) {
		t.Fatalf("prior authority was misclassified as never-started cancellation: %+v", decision)
	}
	if generatorCalled || len(store.execCalls) != 1 {
		t.Fatalf("expired cancellation allocated authority or skipped release: generated=%t execs=%d", generatorCalled, len(store.execCalls))
	}
	if !strings.Contains(store.execCalls[0].sql, "release_reason = 'LEASE_EXPIRED'") {
		t.Fatal("expired prior authority was not released")
	}
}

func TestLiveOwnerRetainsCancellationObservationWithoutACK(t *testing.T) {
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	request := testClaimRequest(publishedDigest)
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("owner-token")))
	expires := time.Date(2030, time.July, 16, 13, 0, 0, 0, time.UTC)
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		claimExecutionRow("CANCELLED", "CLAIMED", true, publishedDigest[:], publishedDigest[:], true),
		{values: []any{
			"owner-claim", "command-1", "execution-1", int64(1),
			request.WorkloadIdentity, request.WorkloadSessionID, request.ProducerID,
			int64(1), int64(1), token[:], expires, "CANCELLED", true, expires.Add(-time.Second),
		}},
		{err: pgx.ErrNoRows},
	}}}
	repository, err := newClaimsRepository(store, func() (string, error) { return "unused", nil }, func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimActiveLeaseNoACK || decision.Lease.DesiredState != runtimedomain.DesiredCancelled || decision.Lease.Fence.Token != token {
		t.Fatalf("live owner lost cancellation observation or received an ACK disposition: %+v", decision)
	}
}

func TestAbortClaimUsesFullFenceAndDurableDisposition(t *testing.T) {
	fence := testValidationFrame(t).Fence
	tests := []struct {
		disposition executionapp.ClaimAbortDisposition
		wantState   string
	}{
		{disposition: executionapp.ClaimAbortInputResolutionRetry, wantState: "DISPATCHED"},
		{disposition: executionapp.ClaimAbortInputResolutionExhausted, wantState: "QUARANTINED"},
		{disposition: executionapp.ClaimAbortInputManifestInvalid, wantState: "QUARANTINED"},
	}
	for _, test := range tests {
		t.Run(string(test.disposition), func(t *testing.T) {
			store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{values: []any{fence.ExecutionID}}}}}
			repository, err := newClaimsRepository(
				store,
				func() (string, error) { return "unused", nil },
				func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.AbortClaim(context.Background(), fence, test.disposition); err != nil {
				t.Fatal(err)
			}
			if len(store.rowCalls) != 1 {
				t.Fatalf("abort did not use one atomic statement: %d", len(store.rowCalls))
			}
			call := store.rowCalls[0]
			for _, predicate := range []string{
				"j.command_id = $3",
				"c.workload_identity = $4",
				"c.workload_session_id = $5",
				"c.producer_id = $6",
				"c.claim_attempt = $7",
				"c.lease_epoch = $8",
				"c.fence_token = $9",
				"c.released_at IS NULL",
				"c.lease_expires_at > clock_timestamp()",
			} {
				if !strings.Contains(call.sql, predicate) {
					t.Fatalf("abort SQL is missing fence predicate %q", predicate)
				}
			}
			if got := call.args[9]; got != string(test.disposition) {
				t.Fatalf("abort lost durable reason: %v", got)
			}
			if got := call.args[10]; got != test.wantState {
				t.Fatalf("abort used wrong durable state: %v", got)
			}
		})
	}
}

func testClaimRequest(digest runtimedomain.Digest) executionapp.ClaimRequest {
	return executionapp.ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: digest,
		WorkloadIdentity:     "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID:    "workload-1",
		ProducerID:           "worker-1",
	}
}

func TestBeginExecutionLostResponseReplayDoesNotTransitionTwice(t *testing.T) {
	fence := runtimedomain.Fence{
		CommandID:         "command-1",
		ExecutionID:       "execution-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID: "workload-1",
		ProducerID:        "worker-1",
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("begin-token"))),
	}
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: []any{"CLAIMED", "RUNNING", "NOT_STARTED", true}},
			{values: []any{"RUNNING", "RUNNING", "MAY_HAVE_STARTED", true}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}

	first, err := repository.BeginExecution(context.Background(), fence)
	if err != nil || first != executionapp.BeginExecutionStartedNow {
		t.Fatalf("first begin disposition=%q err=%v", first, err)
	}
	replay, err := repository.BeginExecution(context.Background(), fence)
	if err != nil || replay != executionapp.BeginExecutionAlreadyStarted {
		t.Fatalf("lost-response replay disposition=%q err=%v", replay, err)
	}
	if store.txCalls != 2 || len(store.rowCalls) != 2 || len(store.execCalls) != 1 {
		t.Fatalf("begin replay mutated more than once: tx=%d rows=%d updates=%d", store.txCalls, len(store.rowCalls), len(store.execCalls))
	}
	for _, predicate := range []string{
		"j.command_id = $3",
		"c.workload_identity = $4",
		"c.workload_session_id = $5",
		"c.producer_id = $6",
		"c.claim_attempt = $7",
		"c.lease_epoch = $8",
		"c.fence_token = $9",
		"c.released_at IS NULL",
		"FOR UPDATE OF j, c",
	} {
		if !strings.Contains(store.rowCalls[0].sql, predicate) {
			t.Fatalf("begin SQL is missing exact fence predicate %q", predicate)
		}
	}
}

func TestBeginExecutionRejectsExpiredClaimWithoutMutation(t *testing.T) {
	fence := runtimedomain.Fence{
		CommandID:         "command-1",
		ExecutionID:       "execution-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID: "workload-1",
		ProducerID:        "worker-1",
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("expired-token"))),
	}
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"CLAIMED", "RUNNING", "NOT_STARTED", false}},
	}}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginExecution(context.Background(), fence); !errors.Is(err, runtimedomain.ErrLeaseExpired) {
		t.Fatalf("expired begin error=%v", err)
	}
	if len(store.execCalls) != 0 {
		t.Fatal("expired begin mutated execution state")
	}
}

func TestBeginExecutionResumesOnlyDurablyPreSubmissionRunningState(t *testing.T) {
	fence := runtimedomain.Fence{
		CommandID:         "command-1",
		ExecutionID:       "execution-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID: "workload-1",
		ProducerID:        "worker-1",
		ClaimAttempt:      2,
		LeaseEpoch:        2,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("resume-token"))),
	}
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"RUNNING", "RUNNING", "PREPARING", true}},
	}}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := repository.BeginExecution(context.Background(), fence)
	if err != nil || disposition != executionapp.BeginExecutionStartedNow {
		t.Fatalf("pre-submission resume disposition=%q err=%v", disposition, err)
	}
	if len(store.execCalls) != 0 {
		t.Fatal("pre-submission resume rewrote durable start state")
	}
}

func TestAuthorizeInvocationGrantsOnceAndFailsClosedOnReplay(t *testing.T) {
	fence := runtimedomain.Fence{
		CommandID:         "command-1",
		ExecutionID:       "execution-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID: "workload-1",
		ProducerID:        "worker-1",
		ClaimAttempt:      2,
		LeaseEpoch:        2,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("invoke-token"))),
	}
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: []any{"RUNNING", "RUNNING", "PREPARING", true}},
			{values: []any{"RUNNING", "RUNNING", "MAY_HAVE_STARTED", true}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repository.AuthorizeInvocation(context.Background(), fence)
	if err != nil || first != executionapp.AuthorizeInvocationNow {
		t.Fatalf("first invocation authorization=%q err=%v", first, err)
	}
	replay, err := repository.AuthorizeInvocation(context.Background(), fence)
	if err != nil || replay != executionapp.AuthorizeInvocationAlready {
		t.Fatalf("invocation replay=%q err=%v", replay, err)
	}
	if len(store.execCalls) != 1 {
		t.Fatalf("invocation authorization mutated %d times", len(store.execCalls))
	}
	for _, predicate := range []string{
		"j.command_id = $3",
		"c.workload_identity = $4",
		"c.workload_session_id = $5",
		"c.producer_id = $6",
		"c.claim_attempt = $7",
		"c.lease_epoch = $8",
		"c.fence_token = $9",
		"c.released_at IS NULL",
		"FOR UPDATE OF j, c",
	} {
		if !strings.Contains(store.rowCalls[0].sql, predicate) {
			t.Fatalf("invocation SQL is missing exact fence predicate %q", predicate)
		}
	}
}

func TestAuthorizeInvocationRejectsCancellationAndStaleGeneration(t *testing.T) {
	fence := runtimedomain.Fence{
		CommandID:         "command-1",
		ExecutionID:       "execution-1",
		Generation:        2,
		WorkloadIdentity:  "spiffe://elitea.test/workload/worker-1",
		WorkloadSessionID: "workload-1",
		ProducerID:        "worker-1",
		ClaimAttempt:      2,
		LeaseEpoch:        2,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("cancel-token"))),
	}
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{"RUNNING", "CANCELLED", "PREPARING", true}},
		{err: pgx.ErrNoRows},
	}}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "unused", nil },
		func() (runtimedomain.FenceToken, error) { return runtimedomain.FenceToken{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AuthorizeInvocation(context.Background(), fence); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("cancellation race authorization error=%v", err)
	}
	stale := fence
	stale.Generation++
	if _, err := repository.AuthorizeInvocation(context.Background(), stale); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("stale generation authorization error=%v", err)
	}
	if len(store.execCalls) != 0 {
		t.Fatal("cancellation/stale generation mutated invocation state")
	}
}

func TestExpiredPreSubmissionIndexClaimReceivesExecutableReplacement(t *testing.T) {
	digest := runtimedomain.SHA256([]byte("published-pre-submission-envelope"))
	oldToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("old-pre-submission-token")))
	newToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("new-pre-submission-token")))
	observedAt := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := observedAt.Add(executionapp.MaxClaimLeaseTTLMillis.Duration())
	executionRow := claimExecutionRow("RUNNING", "RUNNING", true, digest[:], digest[:], true)
	executionRow.values[3] = "PREPARING"
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			executionRow,
			{values: []any{
				"old-claim", "command-1", "execution-1", int64(1),
				"spiffe://elitea.test/workload/old", "old-session", "old-producer",
				int64(1), int64(1), oldToken[:],
				observedAt.Add(-time.Minute), "RUNNING", false, observedAt,
			}},
			{values: []any{int64(2)}},
			{values: []any{expiresAt, observedAt, int64(0)}},
			{err: pgx.ErrNoRows},
			{values: []any{false}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "replacement-claim", nil },
		func() (runtimedomain.FenceToken, error) { return newToken, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(digest)
	request.CapabilityID = executiondomain.IndexIngestCapability
	request.WorkloadIdentity = "spiffe://elitea.test/workload/replacement"
	request.WorkloadSessionID = "replacement-session"
	request.ProducerID = "replacement-producer"

	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimAccepted || decision.Lease.ClaimID != "replacement-claim" {
		t.Fatalf("recoverable pre-submission work was terminalized: %+v", decision)
	}
}

func TestExpiredRunningIndexClaimReturnsRecoveryOnlyReplacement(t *testing.T) {
	digest := runtimedomain.SHA256([]byte("published-index-envelope"))
	oldToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("old-running-token")))
	newToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("new-running-token")))
	observedAt := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := observedAt.Add(executionapp.MaxClaimLeaseTTLMillis.Duration())
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "RUNNING", true, digest[:], digest[:], true),
			{values: []any{
				"old-claim", "command-1", "execution-1", int64(1),
				"spiffe://elitea.test/workload/old", "old-session", "old-producer",
				int64(1), int64(1), oldToken[:],
				observedAt.Add(-time.Minute), "RUNNING", false, observedAt,
			}},
			{values: []any{int64(2)}},
			{values: []any{expiresAt, observedAt, int64(0)}},
			{err: pgx.ErrNoRows},
			{values: []any{false}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "replacement-claim", nil },
		func() (runtimedomain.FenceToken, error) { return newToken, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(digest)
	request.CapabilityID = executiondomain.IndexIngestCapability
	request.WorkloadIdentity = "spiffe://elitea.test/workload/replacement"
	request.WorkloadSessionID = "replacement-session"
	request.ProducerID = "replacement-producer"

	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRecoverAmbiguousInvocationNoACK || decision.Lease.ClaimID != "replacement-claim" || decision.SettlementRecovery != nil {
		t.Fatalf("expired RUNNING claim received executable authority: %+v", decision)
	}
	if decision.Lease.Fence.Token != newToken || decision.Lease.Fence.ClaimAttempt != 2 {
		t.Fatalf("replacement recovery fence is malformed: %+v", decision.Lease.Fence)
	}
}

func TestExpiredCancelledRunningIndexClaimRecoversWithoutDurableOutput(t *testing.T) {
	digest := runtimedomain.SHA256([]byte("published-cancelled-index-envelope"))
	oldToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("old-cancelled-running-token")))
	newToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("new-cancelled-running-token")))
	observedAt := time.Date(2026, time.July, 27, 13, 0, 0, 0, time.UTC)
	expiresAt := observedAt.Add(executionapp.MaxClaimLeaseTTLMillis.Duration())
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("CANCELLED", "RUNNING", true, digest[:], digest[:], true),
			{values: []any{
				"old-claim", "command-1", "execution-1", int64(1),
				"spiffe://elitea.test/workload/old", "old-session", "old-producer",
				int64(1), int64(1), oldToken[:],
				observedAt.Add(-time.Minute), "CANCELLED", false, observedAt,
			}},
			// No output_inbox record exists, so only an expired predecessor claim
			// can authorize the cancellation-only recovery fence.
			{values: []any{false}},
			{values: []any{true}},
			{values: []any{int64(2)}},
			{values: []any{expiresAt, observedAt, int64(0)}},
			{err: pgx.ErrNoRows},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	repository, err := newClaimsRepository(
		store,
		func() (string, error) { return "cancelled-replacement-claim", nil },
		func() (runtimedomain.FenceToken, error) { return newToken, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testClaimRequest(digest)
	request.CapabilityID = executiondomain.IndexIngestCapability
	request.WorkloadIdentity = "spiffe://elitea.test/workload/replacement"
	request.WorkloadSessionID = "replacement-session"
	request.ProducerID = "replacement-producer"

	decision, err := repository.ClaimValidation(context.Background(), request, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRecoverRunningNoACK || decision.Lease.DesiredState != runtimedomain.DesiredCancelled || decision.Lease.ClaimID != "cancelled-replacement-claim" || decision.SettlementRecovery != nil {
		t.Fatalf("cancelled RUNNING execution did not receive recovery-only authority: %+v", decision)
	}
	if decision.ClaimHandoffWatermark != 0 || decision.Lease.Fence.Token != newToken || decision.Lease.Fence.ClaimAttempt != 2 {
		t.Fatalf("cancelled recovery lease lost its exact handoff boundary: %+v", decision)
	}
	if len(store.rowCalls) != 7 || !strings.Contains(store.rowCalls[2].sql, "output_inbox") || !strings.Contains(store.rowCalls[3].sql, "release_reason = 'LEASE_EXPIRED'") {
		t.Fatalf("cancelled recovery was not limited to an expired predecessor without durable output: rows=%d", len(store.rowCalls))
	}
}

func claimExecutionRow(desiredState, jobState string, published bool, preparedDigest, publishedDigest []byte, authorityGranted bool) scriptedRow {
	invocationState := "NOT_STARTED"
	switch jobState {
	case "RUNNING", "SETTLING", "SUCCEEDED", "FAILED":
		invocationState = "MAY_HAVE_STARTED"
	}
	return scriptedRow{values: []any{
		"command-1",
		desiredState,
		jobState,
		invocationState,
		"",
		"outbox-1",
		published,
		preparedDigest,
		publishedDigest,
		false,
		"",
		false,
		authorityGranted,
		int64(1),
	}}
}

func TestScanLeaseKeepsAuthenticatedIdentityDistinctFromProducer(t *testing.T) {
	token := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("token")))
	expires := time.Date(2026, time.July, 16, 13, 0, 0, 0, time.UTC)
	lease, live, err := scanLease(scriptedRow{values: []any{
		"claim-1",
		"command-1",
		"execution-1",
		int64(1),
		"spiffe://elitea.test/worker/1",
		"session-1",
		"producer-alias",
		int64(2),
		int64(3),
		token[:],
		expires,
		"RUNNING",
		true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !live || lease.Fence.WorkloadIdentity != "spiffe://elitea.test/worker/1" || lease.Fence.ProducerID != "producer-alias" || lease.Fence.WorkloadIdentity == lease.Fence.ProducerID {
		t.Fatalf("authenticated identity was conflated with producer: %+v", lease.Fence)
	}
}

func TestExpiredClaimRecoversTerminalOutputAcrossProducerReplacement(t *testing.T) {
	terminalFrame := testValidationFrame(t)
	oldFence := terminalFrame.Fence
	replacementIdentity := "spiffe://elitea.test/workload/replacement"
	replacementProducer := "replacement-producer"
	publishedDigest := runtimedomain.SHA256([]byte("published-signed-envelope"))
	newToken := runtimedomain.FenceToken(runtimedomain.SHA256([]byte("new-token")))
	leaseExpires := time.Date(2030, time.July, 16, 13, 0, 0, 0, time.UTC)
	claimStore := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			claimExecutionRow("RUNNING", "RUNNING", true, publishedDigest[:], publishedDigest[:], true),
			{values: []any{
				"old-claim", "command-1", "execution-1", int64(1),
				oldFence.WorkloadIdentity, oldFence.WorkloadSessionID, oldFence.ProducerID,
				int64(1), int64(1), oldFence.Token[:],
				time.Date(2026, time.July, 16, 11, 0, 0, 0, time.UTC), "RUNNING", false,
				time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
			}},
			{values: []any{int64(2)}},
			{values: []any{leaseExpires, leaseExpires.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration()), int64(0)}},
			{values: []any{
				terminalFrame.Settlement.ProposalID,
				string(terminalFrame.Settlement.Outcome),
				terminalFrame.LogicalOutputID,
				terminalFrame.EventID,
				int64(terminalFrame.Sequence),
				terminalFrame.PayloadDigest[:],
				terminalFrame.EncodedSettlement,
				terminalFrame.Settlement.ProposalDigest[:],
				terminalFrame.Settlement.IdempotencyKey,
				int64(1),
			}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1")},
	}}
	claims, err := newClaimsRepository(
		claimStore,
		func() (string, error) { return "new-claim", nil },
		func() (runtimedomain.FenceToken, error) { return newToken, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := claims.ClaimValidation(context.Background(), executionapp.ClaimRequest{
		CommandID:            "command-1",
		OutboxID:             "outbox-1",
		ExecutionID:          "execution-1",
		Generation:           1,
		CapabilityID:         executiondomain.ConfigurationValidationCapability,
		SignedEnvelopeDigest: publishedDigest,
		WorkloadIdentity:     replacementIdentity,
		WorkloadSessionID:    "replacement-session",
		ProducerID:           replacementProducer,
	}, executionapp.MaxClaimLeaseTTLMillis)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Disposition != executionapp.ClaimRecoverTerminalACK || decision.SettlementRecovery == nil || decision.SettlementRecovery.Proposal == nil {
		t.Fatalf("terminal output was not recovered: %+v", decision)
	}
	recoveryProposal := *decision.SettlementRecovery.Proposal
	if recoveryProposal.Fence != decision.Lease.Fence || recoveryProposal.Fence.ClaimAttempt != 2 {
		t.Fatalf("recovery proposal was not rebound to the authorized replacement fence: %+v", recoveryProposal.Fence)
	}
	if decision.Lease.Fence.WorkloadIdentity != replacementIdentity || decision.Lease.Fence.ProducerID != replacementProducer || decision.Lease.Fence.WorkloadIdentity == oldFence.WorkloadIdentity || decision.Lease.Fence.ProducerID == oldFence.ProducerID {
		t.Fatalf("replacement claim remained bound to the failed pod identity: %+v", decision.Lease.Fence)
	}
	if decision.Lease.Fence.LeaseEpoch != 2 {
		t.Fatalf("replacement claim reused the predecessor lease epoch: %+v", decision.Lease.Fence)
	}
	recoverySQL := claimStore.rowCalls[4].sql
	if !strings.Contains(recoverySQL, "source_claim.release_reason = 'LEASE_EXPIRED'") {
		t.Fatal("terminal recovery query lacks an expired-predecessor handoff predicate")
	}
	branchStart := strings.Index(recoverySQL, "  AND (\n      (")
	branchSplit := strings.Index(recoverySQL, "\n      )\n      OR\n      (")
	if branchStart < 0 || branchSplit <= branchStart {
		t.Fatalf("terminal recovery authority branches are not explicit: %s", recoverySQL)
	}
	prefix := recoverySQL[:branchStart]
	currentClaim := recoverySQL[branchStart:branchSplit]
	predecessor := recoverySQL[branchSplit:]
	if strings.Contains(prefix, "output_inbox.workload_identity = $3") || strings.Contains(prefix, "output_inbox.producer_id = $4") {
		t.Fatal("expired-predecessor recovery remained globally pod-identity bound")
	}
	for _, predicate := range []string{
		"output_inbox.claim_attempt = $5",
		"output_inbox.lease_epoch = $6",
		"output_inbox.fence_token = $7",
		"output_inbox.workload_session_id = $8",
		"output_inbox.workload_identity = $3",
		"output_inbox.producer_id = $4",
	} {
		if !strings.Contains(currentClaim, predicate) {
			t.Fatalf("exact current-claim recovery lost %q", predicate)
		}
	}
	if strings.Contains(predecessor, "output_inbox.workload_identity = $3") || strings.Contains(predecessor, "output_inbox.producer_id = $4") || !strings.Contains(predecessor, "output_inbox.claim_attempt < $5") {
		t.Fatal("expired predecessor is not strictly earlier and independent of pod identity")
	}

	settlementStore := &scriptedStore{scriptedExecutor: &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: []any{"command-1"}},
			{err: pgx.ErrNoRows},
			{values: []any{"new-claim"}},
			{values: []any{terminalFrame.EventID, payloadTypeConfigurationValidation, []byte(nil)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 1"),
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}}
	settlements, err := newSettlementsRepository(settlementStore, func() (string, error) { return "receipt-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := settlements.PrepareSettlement(context.Background(), recoveryProposal)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ID != "receipt-1" || receipt.Outcome != executionapp.SettlementSucceeded {
		t.Fatalf("unexpected recovery settlement receipt: %+v", receipt)
	}
	if !strings.Contains(settlementStore.rowCalls[3].sql, "source_claim.release_reason = 'LEASE_EXPIRED'") || !strings.Contains(settlementStore.rowCalls[3].sql, "o.claim_attempt < $12") {
		t.Fatal("settlement query lacks bounded predecessor handoff predicates")
	}
}
