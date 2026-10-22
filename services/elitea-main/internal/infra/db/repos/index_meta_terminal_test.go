package repos

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"google.golang.org/protobuf/proto"
)

func TestCurrentIndexMetaTerminalBindingUsesFrozenAdmissionEvidence(t *testing.T) {
	frozen := []byte(`{"id":19,"type":"github","settings":{"pgvector_configuration":{"__elitea_frozen_configuration_v1":true}}}`)
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		int32(7),
		"13",
		int32(19),
		"Docs",
		"meta-1",
		"execution-1",
		int64(3),
		int64(8),
		frozen,
	}}}}
	repository, err := newCurrentIndexMetaTerminalBindingsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := repository.LoadCurrentIndexMetaTerminalBinding(context.Background(), "execution-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ResourceProjectID != 7 || binding.ActorUserID != 13 ||
		binding.ToolkitID != 19 || binding.IndexName != "Docs" ||
		binding.MetaID != "meta-1" || binding.ExecutionID != "execution-1" ||
		binding.Generation != 3 || binding.IndexGeneration != 8 ||
		string(binding.ToolkitConfiguration) != string(frozen) {
		t.Fatalf("binding=%+v", binding)
	}
	if len(store.rowCalls) != 1 ||
		len(store.rowCalls[0].args) != 2 ||
		store.rowCalls[0].args[0] != "execution-1" ||
		store.rowCalls[0].args[1] != int64(3) {
		t.Fatalf("calls=%+v", store.rowCalls)
	}
	for _, predicate := range []string{
		"i.index_meta_id",
		"i.index_generation",
		"e.entry_id = i.toolkit_configuration_entry_id",
		"j.capability_id = 'index.ingest.v1'",
		"i.index_meta_initialized_at IS NOT NULL",
	} {
		if !strings.Contains(store.rowCalls[0].sql, predicate) {
			t.Fatalf("terminal binding SQL is missing %q", predicate)
		}
	}
}

func TestCurrentIndexMetaTerminalEffectClaimsOnlyBoundedPendingRows(t *testing.T) {
	cancelledAt := time.Date(2026, time.July, 26, 12, 13, 14, 0, time.UTC)
	deadlineAt := cancelledAt.Add(time.Minute)
	store := &scriptedExecutor{rowsResult: &scriptedRows{rows: []scriptedRow{
		{values: []any{
			"cancelled", int64(1), "cancelled", cancelledAt,
		}},
		{values: []any{
			"deadline",
			int64(2),
			"failed",
			deadlineAt,
		}},
	}}, rowResults: []scriptedRow{{values: []any{"", nil, "DEADLINE_EXCEEDED"}}}}
	repository, err := newCurrentIndexMetaTerminalBindingsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := repository.ClaimPendingTerminalEffects(
		context.Background(),
		"claim-1",
		8,
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 ||
		claims[0].State != indexingapp.CurrentIndexMetaCancelled ||
		claims[0].SafeError != "" || claims[0].ClaimToken != "claim-1" ||
		claims[1].State != indexingapp.CurrentIndexMetaFailed ||
		claims[1].SafeError == "" || claims[1].ClaimToken != "claim-1" {
		t.Fatalf("claims=%+v", claims)
	}
	query := store.queryCalls[0]
	for _, predicate := range []string{
		"WITH candidates AS",
		"FROM elitea_runtime.index_ingest_jobs AS i",
		"i.capability_id = 'index.ingest.v1'",
		"i.index_meta_terminal_status = 'PENDING'",
		"i.index_meta_terminal_next_attempt_at <= clock_timestamp()",
		"LIMIT $1",
		"FOR UPDATE OF i SKIP LOCKED",
		"index_meta_terminal_claim_expires_at",
	} {
		if !strings.Contains(query.sql, predicate) {
			t.Fatalf("terminal effect claim is missing %q", predicate)
		}
	}
	for _, forbidden := range []string{
		"terminal_evidence",
		"terminal_sources",
		"output_inbox",
		"command_outbox",
		"index_meta_terminal_status IS NULL",
	} {
		if strings.Contains(query.sql, forbidden) {
			t.Fatalf("steady terminal claim scans source history via %q", forbidden)
		}
	}
	if len(store.rowCalls) != 1 {
		t.Fatalf("safe-error source lookups=%d, want one failed claim", len(store.rowCalls))
	}
	safeSource := store.rowCalls[0]
	for _, predicate := range []string{
		"o.execution_id = $1",
		"o.generation = $2",
		"o.occurred_at = $3",
		"o.retired_at = $3",
		"o.settlement_outcome = 'FAILED'",
		"o.retirement_code = 'DEADLINE_EXCEEDED'",
	} {
		if !strings.Contains(safeSource.sql, predicate) {
			t.Fatalf("exact safe-error lookup is missing %q", predicate)
		}
	}
	if len(safeSource.args) != 3 ||
		safeSource.args[0] != "deadline" ||
		safeSource.args[1] != int64(2) ||
		safeSource.args[2] != deadlineAt {
		t.Fatalf("safe-error lookup args=%#v", safeSource.args)
	}
}

func TestCurrentIndexMetaTerminalSafeErrorLoadsOnlyTypedIndexSummary(t *testing.T) {
	result := &runtimev1.IndexIngestResultV1{
		ResultSummary: &runtimev1.IndexIngestSummaryV1{
			Status:  runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_ERROR,
			Message: "Indexing failed before completion.",
		},
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := terminalSafeError(payloadTypeIndexIngestResult, payload, ""); got != result.GetResultSummary().GetMessage() {
		t.Fatalf("typed safe error=%q", got)
	}
	canary := "RAW_SDK_CREDENTIAL_CANARY"
	if got := terminalSafeError(payloadTypeIndexIngestResult, []byte(canary), ""); got == canary {
		t.Fatal("malformed index payload leaked raw SDK text")
	}
}

func TestCurrentIndexMetaTerminalEffectResolutionOwnsClaimToken(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{"APPLIED"}}}}
	repository, err := newCurrentIndexMetaTerminalBindingsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	claim := indexingapp.CurrentIndexMetaTerminalClaim{
		CurrentIndexMetaTerminalRequest: indexingapp.CurrentIndexMetaTerminalRequest{
			ExecutionID: "execution-1",
			Generation:  3,
			State:       indexingapp.CurrentIndexMetaFailed,
			OccurredAt:  time.Now(),
			SafeError:   "A dependency is unavailable.",
		},
		ClaimToken: "claim-1",
	}
	if err := repository.ResolveTerminalEffect(
		context.Background(),
		claim,
		indexingapp.CurrentIndexMetaTerminalApplied,
	); err != nil {
		t.Fatal(err)
	}
	for _, predicate := range []string{
		"index_meta_terminal_status = $5",
		"index_meta_terminal_claim_token = $6",
		"index_meta_terminalized_at = COALESCE",
	} {
		if !strings.Contains(store.rowCalls[0].sql, predicate) {
			t.Fatalf("terminal effect resolution is missing %q", predicate)
		}
	}
}

func TestCurrentIndexMetaTerminalEffectSupersedesOnlyDurablyNewerInitializedIdentity(t *testing.T) {
	occurredAt := time.Date(2026, time.July, 26, 12, 13, 14, 0, time.UTC)
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{true, true}}}}
	repository, err := newCurrentIndexMetaTerminalBindingsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	claim := indexingapp.CurrentIndexMetaTerminalClaim{
		CurrentIndexMetaTerminalRequest: indexingapp.CurrentIndexMetaTerminalRequest{
			ExecutionID: "execution-a",
			Generation:  1,
			State:       indexingapp.CurrentIndexMetaCancelled,
			OccurredAt:  occurredAt,
		},
		ClaimToken: "claim-a",
	}
	superseded, err := repository.SupersedeTerminalEffectIfNewerInitialized(
		context.Background(),
		claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !superseded || len(store.rowCalls) != 1 {
		t.Fatalf("superseded=%v calls=%+v", superseded, store.rowCalls)
	}
	call := store.rowCalls[0]
	for _, predicate := range []string{
		"WITH stale AS MATERIALIZED",
		"resolved AS",
		"j.resource_project_id",
		"i.toolkit_id",
		"i.index_name",
		"i.index_meta_terminal_claim_token = $5",
		"index_meta_terminal_status = 'SUPERSEDED'",
		"newer_job.resource_project_id = stale.resource_project_id",
		"newer_index.toolkit_id = stale.toolkit_id",
		"newer_index.index_name = stale.index_name",
		"newer_index.index_meta_initialized_at IS NOT NULL",
		"newer_index.index_generation > stale.index_generation",
		"newer_index.index_generation = stale.index_generation",
		"(newer_job.admitted_at, newer_job.execution_id)",
		"> (stale.admitted_at, stale.execution_id)",
		"EXISTS (SELECT 1 FROM stale)",
		"EXISTS (SELECT 1 FROM resolved)",
	} {
		if !strings.Contains(call.sql, predicate) {
			t.Fatalf("terminal supersession SQL is missing %q", predicate)
		}
	}
	if len(call.args) != 5 ||
		call.args[0] != claim.ExecutionID ||
		call.args[1] != int64(claim.Generation) ||
		call.args[2] != string(claim.State) ||
		call.args[3] != occurredAt ||
		call.args[4] != claim.ClaimToken {
		t.Fatalf("terminal supersession args=%#v", call.args)
	}
}

func TestCurrentIndexMetaTerminalEffectDoesNotInferSupersessionFromNoMatch(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{true, false}}}}
	repository, err := newCurrentIndexMetaTerminalBindingsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := repository.SupersedeTerminalEffectIfNewerInitialized(
		context.Background(),
		indexingapp.CurrentIndexMetaTerminalClaim{
			CurrentIndexMetaTerminalRequest: indexingapp.CurrentIndexMetaTerminalRequest{
				ExecutionID: "execution-latest",
				Generation:  1,
				State:       indexingapp.CurrentIndexMetaCancelled,
				OccurredAt:  time.Date(2026, time.July, 26, 12, 13, 14, 0, time.UTC),
			},
			ClaimToken: "claim-latest",
		},
	)
	if err != nil || superseded {
		t.Fatalf("superseded=%v err=%v", superseded, err)
	}
}

func TestCurrentIndexMetaTerminalEffectRejectsLostClaimBeforeExternalWrite(t *testing.T) {
	store := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{false, false}}}}
	repository, err := newCurrentIndexMetaTerminalBindingsRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := repository.SupersedeTerminalEffectIfNewerInitialized(
		context.Background(),
		indexingapp.CurrentIndexMetaTerminalClaim{
			CurrentIndexMetaTerminalRequest: indexingapp.CurrentIndexMetaTerminalRequest{
				ExecutionID: "execution-reclaimed",
				Generation:  1,
				State:       indexingapp.CurrentIndexMetaCancelled,
				OccurredAt:  time.Date(2026, time.July, 26, 12, 13, 14, 0, time.UTC),
			},
			ClaimToken: "stale-claim",
		},
	)
	if !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) || superseded {
		t.Fatalf("superseded=%v err=%v", superseded, err)
	}
}

func TestIndexMetaTerminalEffectMigrationSeedsOldSourceBackedRows(t *testing.T) {
	migration, err := os.ReadFile("../../../../migrations/shared/0043_index_meta_terminal_effect.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, fragment := range []string{
		"idempotent seed for the single-instance",
		"rolling",
		"mixed-version production rollout requires a separate rerunnable post-cutover",
		"ADD COLUMN index_meta_terminal_state TEXT",
		"ADD COLUMN index_meta_terminal_occurred_at TIMESTAMPTZ",
		"ADD COLUMN index_meta_terminal_status TEXT",
		"ADD COLUMN index_meta_terminal_claim_token TEXT",
		"ADD COLUMN index_meta_terminal_next_attempt_at TIMESTAMPTZ",
		"ADD COLUMN index_meta_terminalized_at TIMESTAMPTZ",
		"index_ingest_jobs_meta_terminal_identity",
		"index_ingest_jobs_meta_terminal_requires_initialization",
		"WITH terminal_evidence AS",
		"terminal_sources AS",
		"UPDATE elitea_runtime.index_ingest_jobs AS i",
		"o.projected_at IS NOT NULL",
		"o.authority_granted_at IS NULL",
		"i.capability_id = 'index.ingest.v1'",
		"i.index_meta_initialized_at IS NOT NULL",
		"i.index_meta_terminal_status IS NULL",
		"index_meta_terminal_status = 'PENDING'",
		"index_meta_terminal_attempt_count = 0",
		"WHERE index_meta_terminal_status = 'PENDING'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("terminal effect migration is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"command_outbox_retired_no_authority_terminal_idx",
		"output_inbox_projected_runtime_terminal_idx",
		"payload_bytes",
	} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("migration retains recurring source scan/index via %q", forbidden)
		}
	}
}

func TestIndexMetaTerminalSupersessionMigrationProvidesOrderedLookup(t *testing.T) {
	migration, err := os.ReadFile("../../../../migrations/shared/0044_index_meta_terminal_supersession.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, fragment := range []string{
		"CREATE INDEX execution_jobs_index_identity_order_idx",
		"resource_project_id, admitted_at, execution_id, generation",
		"WHERE capability_id = 'index.ingest.v1'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("terminal supersession migration is missing %q", fragment)
		}
	}
}

func TestIndexGenerationMigrationDefinesTargetScopedAuthority(t *testing.T) {
	migration, err := os.ReadFile("../../../../migrations/shared/0046_index_generation.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(migration)
	for _, fragment := range []string{
		"ADD COLUMN index_generation BIGINT",
		"SET index_generation = 1",
		"ALTER COLUMN index_generation SET NOT NULL",
		"CREATE TABLE elitea_runtime.index_generation_counters",
		"PRIMARY KEY (resource_project_id, toolkit_id, index_name)",
		"max(i.index_generation)",
		"ON CONFLICT (resource_project_id, toolkit_id, index_name) DO UPDATE",
		"index_ingest_jobs_initialized_generation_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("index generation migration is missing %q", fragment)
		}
	}
}

// TestPostgresCurrentIndexMetaLogicalGenerationSupersession crosses the real
// PostgreSQL migration, allocation, claim ownership and supersession
// transition. It proves runtime generation remains one while the target-scoped
// logical generation is the primary order even when admitted_at is equal;
// it does not cross the terminal effect or the runtime reconciler process.
func TestPostgresCurrentIndexMetaLogicalGenerationSupersession(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	policy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    16,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	terminals, err := NewCurrentIndexMetaTerminalBindingsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first, err := newPostgresIndexAdmissionService(t, jobs, "supersession-a").Submit(
		ctx,
		postgresIndexSubmitRequest("supersession-a", "equal-generation"),
	)
	if err != nil || !first.Created || first.Generation != 1 || first.IndexGeneration != 1 {
		t.Fatalf("first admission=%+v err=%v", first, err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     first.ExecutionID,
		Generation:      first.Generation,
		IndexGeneration: first.IndexGeneration,
		MetaID:          first.IndexMetaID,
		CorrelationID:   first.IndexMetaCorrelationID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'SUCCEEDED',
    settled_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		first.ExecutionID,
		int64(first.Generation),
	); err != nil {
		t.Fatal(err)
	}
	second, err := newPostgresIndexAdmissionService(t, jobs, "supersession-z").Submit(
		ctx,
		postgresIndexSubmitRequest("supersession-z", "equal-generation"),
	)
	if err != nil || !second.Created || second.Generation != 1 || second.IndexGeneration != 2 {
		t.Fatalf("second admission=%+v err=%v", second, err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     second.ExecutionID,
		Generation:      second.Generation,
		IndexGeneration: second.IndexGeneration,
		MetaID:          second.IndexMetaID,
		CorrelationID:   second.IndexMetaCorrelationID,
	}); err != nil {
		t.Fatal(err)
	}
	if first.ExecutionID >= second.ExecutionID {
		t.Fatalf("fixture execution order is not increasing: first=%q second=%q", first.ExecutionID, second.ExecutionID)
	}
	admittedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET admitted_at = $3
WHERE (execution_id, generation) IN (($1, 1), ($2, 1))`,
		first.ExecutionID,
		second.ExecutionID,
		admittedAt,
	); err != nil {
		t.Fatal(err)
	}

	occurredAt := admittedAt.Add(time.Minute)
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_state = 'cancelled',
    index_meta_terminal_occurred_at = $3,
    index_meta_terminal_status = 'PENDING',
    index_meta_terminal_claim_token = $4,
    index_meta_terminal_claim_expires_at = clock_timestamp() + interval '1 minute',
    index_meta_terminal_attempt_count = 1,
    index_meta_terminal_next_attempt_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		first.ExecutionID,
		int64(first.Generation),
		occurredAt,
		"claim-first",
	); err != nil {
		t.Fatal(err)
	}
	firstClaim := indexingapp.CurrentIndexMetaTerminalClaim{
		CurrentIndexMetaTerminalRequest: indexingapp.CurrentIndexMetaTerminalRequest{
			ExecutionID: first.ExecutionID,
			Generation:  first.Generation,
			State:       indexingapp.CurrentIndexMetaCancelled,
			OccurredAt:  occurredAt,
		},
		ClaimToken: "claim-first",
	}
	superseded, err := terminals.SupersedeTerminalEffectIfNewerInitialized(ctx, firstClaim)
	if err != nil || !superseded {
		t.Fatalf("supersede first logical generation=%v err=%v", superseded, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND generation = 1
  AND index_meta_terminal_status = 'SUPERSEDED'
  AND index_meta_terminal_claim_token IS NULL`,
		first.ExecutionID,
	)

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_state = 'cancelled',
    index_meta_terminal_occurred_at = $3,
    index_meta_terminal_status = 'PENDING',
    index_meta_terminal_claim_token = $4,
    index_meta_terminal_claim_expires_at = clock_timestamp() + interval '1 minute',
    index_meta_terminal_attempt_count = 1,
    index_meta_terminal_next_attempt_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2`,
		second.ExecutionID,
		int64(second.Generation),
		occurredAt,
		"claim-second",
	); err != nil {
		t.Fatal(err)
	}
	secondClaim := indexingapp.CurrentIndexMetaTerminalClaim{
		CurrentIndexMetaTerminalRequest: indexingapp.CurrentIndexMetaTerminalRequest{
			ExecutionID: second.ExecutionID,
			Generation:  second.Generation,
			State:       indexingapp.CurrentIndexMetaCancelled,
			OccurredAt:  occurredAt,
		},
		ClaimToken: "claim-second",
	}
	superseded, err = terminals.SupersedeTerminalEffectIfNewerInitialized(ctx, secondClaim)
	if err != nil || superseded {
		t.Fatalf("latest execution superseded=%v err=%v", superseded, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.index_ingest_jobs
WHERE execution_id = $1
  AND generation = 1
  AND index_meta_terminal_status = 'PENDING'
  AND index_meta_terminal_claim_token = 'claim-second'`,
		second.ExecutionID,
	)
}
