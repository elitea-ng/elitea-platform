package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	configurationdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

func TestExecutionJobsRepositoryReplaysMatchingIdempotencyWithoutWriting(t *testing.T) {
	admission := testValidationAdmission()
	admittedAt, deadline := testAdmissionTiming()
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"durable-execution", "durable-command", admission.Record.RequestDigest[:], admittedAt, deadline,
	}}}}}
	repository, err := newExecutionJobsRepository(store, testDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := repository.AdmitValidation(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExecutionID != "durable-execution" || outcome.CommandID != "durable-command" || outcome.Created || outcome.AdmittedAt != admittedAt || outcome.Deadline != deadline {
		t.Fatalf("unexpected replay outcome: %+v", outcome)
	}
	if len(store.execCalls) != 0 || len(store.rowCalls) != 1 {
		t.Fatalf("idempotent replay wrote data: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
	}
}

func TestExecutionJobsRepositoryRejectsIdempotencyDigestConflict(t *testing.T) {
	admission := testValidationAdmission()
	other := runtimedomain.SHA256([]byte("different request"))
	admittedAt, deadline := testAdmissionTiming()
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"durable-execution", "durable-command", other[:], admittedAt, deadline,
	}}}}}
	repository, err := newExecutionJobsRepository(store, testDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.AdmitValidation(context.Background(), admission)
	if !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
	if len(store.execCalls) != 0 {
		t.Fatal("conflicting replay wrote data")
	}
}

func TestExecutionJobsRepositoryRechecksReplayAfterPolicyLockBeforeCapacity(t *testing.T) {
	admission := testValidationAdmission()
	policy := testDispatchPolicy()
	admittedAt, deadline := testAdmissionTiming()
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{policy.MaxOutstanding}},
		{values: []any{"durable-execution", "durable-command", admission.Record.RequestDigest[:], admittedAt, deadline}},
	}}}
	repository, err := newExecutionJobsRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := repository.AdmitValidation(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Created || outcome.ExecutionID != "durable-execution" || outcome.CommandID != "durable-command" || outcome.AdmittedAt != admittedAt || outcome.Deadline != deadline {
		t.Fatalf("unexpected concurrent replay outcome: %+v", outcome)
	}
	if len(store.execCalls) != 1 || len(store.rowCalls) != 3 {
		t.Fatalf("concurrent replay crossed admission writes: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
	}
	if !strings.Contains(store.rowCalls[1].sql, "FOR UPDATE") {
		t.Fatal("capability policy was not locked before replay recheck")
	}
}

func TestExecutionJobsRepositoryRejectsAtCapacityWithoutWritingExecution(t *testing.T) {
	admission := testValidationAdmission()
	policy := testDispatchPolicy()
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{policy.MaxOutstanding}},
		{err: pgx.ErrNoRows},
		{values: []any{policy.MaxOutstanding}},
	}}}
	repository, err := newExecutionJobsRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.AdmitValidation(context.Background(), admission)
	if !errors.Is(err, executionapp.ErrAdmissionCapacityExhausted) {
		t.Fatalf("expected typed capacity rejection, got %v", err)
	}
	var capacityError *executionapp.AdmissionCapacityError
	if !errors.As(err, &capacityError) || !capacityError.Retryable() || capacityError.RetryAfter() <= 0 || capacityError.MaxOutstanding != policy.MaxOutstanding {
		t.Fatalf("capacity rejection lost retry contract: %#v", capacityError)
	}
	if len(store.execCalls) != 1 {
		t.Fatalf("capacity rejection wrote input/job/outbox rows: execs=%d", len(store.execCalls))
	}
	if call := store.rowCalls[3]; !strings.Contains(call.sql, "LIMIT $2") || !strings.Contains(call.sql, "'PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING'") || len(call.args) != 2 || call.args[1] != policy.MaxOutstanding {
		t.Fatalf("active execution count is not bounded by policy: sql=%s args=%v", call.sql, call.args)
	}
}

func TestExecutionJobsRepositoryFailsClosedOnPersistedPolicyMismatch(t *testing.T) {
	admission := testValidationAdmission()
	policy := testDispatchPolicy()
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{policy.MaxOutstanding + 1}},
	}}}
	repository, err := newExecutionJobsRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.AdmitValidation(context.Background(), admission)
	if !errors.Is(err, ErrAdmissionPolicyMismatch) {
		t.Fatalf("expected fail-closed policy mismatch, got %v", err)
	}
	// #966: the operator who meets this error is told both values and the
	// one-step fix, not only that the two differ.
	for _, want := range []string{
		fmt.Sprintf("configured=%d", policy.MaxOutstanding),
		fmt.Sprintf("persisted=%d", policy.MaxOutstanding+1),
		"ELITEA_RUNTIME_MAX_OUTSTANDING",
		"every elitea-main replica",
		"elitea-admission-ctl set -max",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("policy mismatch message lacks %q: %v", want, err)
		}
	}
	if len(store.execCalls) != 1 || len(store.rowCalls) != 2 || !strings.Contains(store.rowCalls[1].sql, "FOR UPDATE") {
		t.Fatalf("policy mismatch crossed admission boundary: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
	}
}

func TestValidationDispatchPolicyAcceptsCapsAboveTheFormer1024Ceiling(t *testing.T) {
	for _, maxOutstanding := range []int64{1025, 2048, MaxSupportedOutstandingJobs} {
		policy := testDispatchPolicy()
		policy.MaxOutstanding = maxOutstanding
		if _, err := newExecutionJobsRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{}}, policy); err != nil {
			t.Fatalf("rejected supported MaxOutstanding=%d: %v", maxOutstanding, err)
		}
	}
}

func TestValidationDispatchPolicyRequiresBoundedPositiveAdmissionCapacity(t *testing.T) {
	for _, maxOutstanding := range []int64{0, -1, MaxSupportedOutstandingJobs + 1} {
		policy := testDispatchPolicy()
		policy.MaxOutstanding = maxOutstanding
		if _, err := newExecutionJobsRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{}}, policy); err == nil {
			t.Fatalf("accepted unsupported MaxOutstanding=%d", maxOutstanding)
		}
	}
}

func TestValidationDispatchPolicyRequiresBoundedMillisecondDeadlineTTL(t *testing.T) {
	for _, deadlineTTL := range []time.Duration{
		0,
		time.Nanosecond,
		time.Millisecond + time.Nanosecond,
		maxValidationDeadlineTTL + time.Millisecond,
	} {
		policy := testDispatchPolicy()
		policy.DeadlineTTL = deadlineTTL
		if _, err := newExecutionJobsRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{}}, policy); err == nil {
			t.Fatalf("accepted unsupported DeadlineTTL=%s", deadlineTTL)
		}
	}

	for _, deadlineTTL := range []time.Duration{minValidationDeadlineTTL, maxValidationDeadlineTTL} {
		policy := testDispatchPolicy()
		policy.DeadlineTTL = deadlineTTL
		if _, err := newExecutionJobsRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{}}, policy); err != nil {
			t.Fatalf("rejected supported DeadlineTTL=%s: %v", deadlineTTL, err)
		}
	}
}

func TestExecutionJobsRepositoryPersistsOneDatabaseAuthoredAdmissionClock(t *testing.T) {
	admission := testValidationAdmission()
	applicationTime := admission.Record.Job.CreatedAt
	policy := testDispatchPolicy()
	admittedAt, deadline := testAdmissionTiming()
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{err: pgx.ErrNoRows},
		{values: []any{policy.MaxOutstanding}},
		{err: pgx.ErrNoRows},
		{values: []any{int64(0)}},
		{values: []any{admittedAt, deadline}},
		{values: []any{admission.Record.Job.ID}},
	}}}
	repository, err := newExecutionJobsRepository(store, policy)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := repository.AdmitValidation(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Created || outcome.AdmittedAt != admittedAt || outcome.Deadline != deadline {
		t.Fatalf("admission did not return database timing: %+v", outcome)
	}
	if len(store.rowCalls) != 6 || len(store.execCalls) != 4 {
		t.Fatalf("unexpected admission statements: rows=%d execs=%d", len(store.rowCalls), len(store.execCalls))
	}
	clockCall := store.rowCalls[4]
	if !strings.Contains(clockCall.sql, "clock_timestamp()") || !strings.Contains(clockCall.sql, "$1::bigint * interval '1 millisecond'") || len(clockCall.args) != 1 || clockCall.args[0] != policy.DeadlineTTL.Milliseconds() {
		t.Fatalf("admission timing is not derived by the database from the bounded TTL: sql=%s args=%v", clockCall.sql, clockCall.args)
	}

	inputCall := store.execCalls[1]
	jobCall := store.rowCalls[5]
	outboxCall := store.execCalls[3]
	if got := inputCall.args[len(inputCall.args)-1]; got != admittedAt {
		t.Fatalf("input bundle created_at=%v, want database admitted_at=%v", got, admittedAt)
	}
	if got := jobCall.args[len(jobCall.args)-1]; got != admittedAt {
		t.Fatalf("job admitted_at=%v, want database admitted_at=%v", got, admittedAt)
	}
	if got := outboxCall.args[7]; got != deadline {
		t.Fatalf("outbox deadline=%v, want database deadline=%v", got, deadline)
	}
	if got := outboxCall.args[len(outboxCall.args)-1]; got != admittedAt {
		t.Fatalf("outbox created_at=%v, want database admitted_at=%v", got, admittedAt)
	}
	for _, call := range []queryCall{inputCall, jobCall, outboxCall} {
		for _, argument := range call.args {
			if timestamp, ok := argument.(time.Time); ok && timestamp.Equal(applicationTime) {
				t.Fatalf("application timestamp crossed the persistence boundary: sql=%s", call.sql)
			}
		}
	}
}

func testDispatchPolicy() ValidationDispatchPolicy {
	return ValidationDispatchPolicy{
		StreamName:        "runtime:commands",
		CapabilityVersion: "1.0.0",
		ResourceClass:     "validation-small",
		IsolationClass:    "tenant",
		Priority:          1,
		DeadlineTTL:       time.Minute,
		LimitsRevision:    "limits-v1",
		MaxOutstanding:    3,
	}
}

func testAdmissionTiming() (time.Time, time.Time) {
	admittedAt := time.Date(2026, time.July, 17, 15, 0, 0, 123_000_000, time.UTC)
	return admittedAt, admittedAt.Add(time.Minute)
}

func testValidationAdmission() executionapp.ValidationAdmission {
	manifest := []byte("deterministic manifest")
	settings := []byte("{}\n")
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	command := configurationdomain.ValidationCommand{
		ConfigurationRevisionID: "revision-1",
		ConfigurationType:       "openapi",
		CatalogRevision:         "catalog-v1",
		CatalogDigest:           runtimedomain.SHA256([]byte("catalog")),
		SchemaID:                "openapi",
		SchemaRevision:          "schema-v1",
		SchemaDigest:            runtimedomain.SHA256([]byte("schema")),
		SettingsEntryID:         "settings",
	}
	return executionapp.ValidationAdmission{
		Command: command,
		Record: executiondomain.Admission{
			IdempotencyScope: "tenant-1/1/actor-1",
			IdempotencyKey:   "request-1",
			RequestDigest:    runtimedomain.SHA256([]byte("request")),
			InputBundle: executiondomain.InputBundle{
				ID:        "bundle-1",
				Version:   "revision-1",
				MediaType: executiondomain.InputBundleManifestMediaType,
				Digest:    runtimedomain.SHA256(manifest),
				Manifest:  manifest,
				Entries: []executiondomain.InputEntry{{
					ID:                    "settings",
					Version:               "revision-1",
					SemanticRole:          "configuration.settings",
					ContentID:             "content-1",
					MediaType:             executiondomain.SettingsJSONMediaType,
					Classification:        "tenant-confidential",
					RequiredGrantAudience: "elitea.runtime.input.read.v1",
					ContentDigest:         runtimedomain.SHA256(settings),
					ContentLength:         int64(len(settings)),
					Content:               settings,
				}},
			},
			Job: executiondomain.Job{
				ID:                  "execution-1",
				CommandID:           "command-1",
				TenantID:            "tenant-1",
				ResourceProjectID:   "1",
				ProjectionProjectID: "1",
				ActorID:             "actor-1",
				CapabilityID:        executiondomain.ConfigurationValidationCapability,
				Generation:          1,
				State:               executiondomain.JobPending,
				CreatedAt:           createdAt,
			},
			Outbox: executiondomain.OutboxRecord{
				ID:          "outbox-1",
				CommandID:   "command-1",
				ExecutionID: "execution-1",
				Generation:  1,
				CreatedAt:   createdAt,
			},
		},
	}
}
