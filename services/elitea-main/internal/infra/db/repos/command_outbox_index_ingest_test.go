package repos

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestListPendingIndexIngestIDsIsCapabilityAndStreamScoped(t *testing.T) {
	executor := &scriptedExecutor{rowsResult: &scriptedRows{rows: []scriptedRow{
		{values: []any{"index-outbox-1"}},
	}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := repository.ListPendingIndexIngestIDs(context.Background(), 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"index-outbox-1"}) {
		t.Fatalf("unexpected pending index IDs: %v", ids)
	}
	if len(executor.queryCalls) != 1 {
		t.Fatalf("query calls = %d", len(executor.queryCalls))
	}
	call := executor.queryCalls[0]
	for _, fragment := range []string{
		"JOIN elitea_runtime.index_ingest_jobs AS i",
		"o.stream_name = $1",
		"j.capability_id = 'index.ingest.v1'",
		"j.generation = 1",
		"o.published_at IS NULL",
		"o.published_at IS NOT NULL",
		"COALESCE(o.last_visibility_at, o.published_at)",
	} {
		if !strings.Contains(call.sql, fragment) {
			t.Fatalf("index pending query is missing %q", fragment)
		}
	}
	if strings.Count(call.sql, "i.index_meta_initialized_at IS NOT NULL") != 2 {
		t.Fatal("both unpublished and visibility-redelivery arms must require initialized index metadata")
	}
	if strings.Count(call.sql, "j.desired_state = 'RUNNING'") != 2 {
		t.Fatal("both unpublished and visibility-redelivery arms must reject cancelled index work")
	}
	if strings.Contains(call.sql, "configuration.validate.v1") || !reflect.DeepEqual(call.args, []any{"runtime:index:commands", int32(1), int64(time.Minute.Milliseconds())}) {
		t.Fatalf("index pending query crossed capability/stream boundary: args=%#v", call.args)
	}
}

func TestLoadPendingIndexIngestJoinsMetadataWithoutEntryContent(t *testing.T) {
	manifestDigest := runtimedomain.SHA256([]byte("index-manifest"))
	embeddingBindingDigest := runtimedomain.SHA256([]byte("embedding-binding"))
	deadline := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"index-outbox-1", "index-command-1", "index-execution-1", int64(1), int64(1),
		"tenant-1", "1", "1", "actor-1",
		"index-bundle-1", "admission:index-bundle-1", "application/x-protobuf", int64(256), manifestDigest[:],
		"1", "indexing", "project", int32(1), deadline, "index-limits-v1", "", "",
		"toolkit-configuration", "tool-parameters", "llm-model", "llm-configuration", "mcp-credential-references",
		"embedding-binding", embeddingBindingDigest[:], int64(1),
		"stream-1", "message-1", "chat_predict", "schedule", "", int32(0),
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := repository.LoadPendingIndexIngest(context.Background(), "index-outbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.OutboxID != "index-outbox-1" || dispatch.InputBundleDigest != manifestDigest ||
		dispatch.ToolkitConfigurationEntryID != "toolkit-configuration" ||
		dispatch.MCPTokensEntryID != "mcp-credential-references" ||
		dispatch.EmbeddingBindingEntryID != "embedding-binding" ||
		dispatch.EmbeddingBindingDigest != embeddingBindingDigest ||
		dispatch.ClientStreamID != "stream-1" ||
		dispatch.ClientMessageID != "message-1" ||
		dispatch.SIOEvent != "chat_predict" ||
		dispatch.Initiator != executiondomain.IndexIngestInitiatorSchedule {
		t.Fatalf("unexpected index dispatch: %+v", dispatch)
	}
	query := executor.rowCalls[0].sql
	for _, fragment := range []string{
		"JOIN elitea_runtime.index_ingest_jobs AS i",
		"JOIN elitea_runtime.input_bundles AS b",
		"LEFT JOIN LATERAL",
		"entry.semantic_role = 'index.embedding_binding'",
		"i.input_bundle_id = j.input_bundle_id",
		"j.capability_id = 'index.ingest.v1'",
		"j.desired_state = 'RUNNING'",
		"i.index_meta_initialized_at IS NOT NULL",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("index dispatch query is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"manifest_bytes", "content_bytes"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("index dispatch query loads data-plane field %q", forbidden)
		}
	}
	// Python mode: the registry is never read, and the command carries no
	// expected embedding space.
	if args := executor.rowCalls[0].args; len(args) != 3 || args[2] != false {
		t.Fatalf("python-mode dispatch args = %#v, want the registry gate off", args)
	}
	if dispatch.ExpectedEmbeddingModel != "" || dispatch.ExpectedEmbeddingDimension != 0 {
		t.Fatalf("python-mode dispatch carries an expected embedding: %+v", dispatch)
	}
}

// Rust mode: the dispatch reads the stamp of the run's registry row and the
// command carries it as the expected embedding space.
func TestLoadPendingIndexIngestCarriesTheRegistryStampInRustMode(t *testing.T) {
	manifestDigest := runtimedomain.SHA256([]byte("index-manifest"))
	deadline := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	row := func(model string, dimension int32) []scriptedRow {
		return []scriptedRow{{values: []any{
			"index-outbox-1", "index-command-1", "index-execution-1", int64(1), int64(1),
			"tenant-1", "1", "1", "actor-1",
			"index-bundle-1", "admission:index-bundle-1", "application/x-protobuf", int64(256), manifestDigest[:],
			"1", "indexing", "project", int32(1), deadline, "index-limits-v1", "", "",
			"toolkit-configuration", "tool-parameters", "", "", "",
			"", []byte{}, int64(0),
			"", "", "", "user", model, dimension,
		}}}
	}
	executor := &scriptedExecutor{rowResults: row("Org/Model.A", 1536)}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	repository.WithIndexRegistry()
	dispatch, err := repository.LoadPendingIndexIngest(context.Background(), "index-outbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.ExpectedEmbeddingModel != "Org/Model.A" || dispatch.ExpectedEmbeddingDimension != 1536 {
		t.Fatalf("expected embedding = %q/%d", dispatch.ExpectedEmbeddingModel, dispatch.ExpectedEmbeddingDimension)
	}
	call := executor.rowCalls[0]
	if len(call.args) != 3 || call.args[2] != true ||
		!strings.Contains(call.sql, "FROM elitea_runtime.index_registry AS r") ||
		!strings.Contains(call.sql, "r.execution_id = j.execution_id") {
		t.Fatalf("rust-mode dispatch query/args = %#v", call.args)
	}

	// An unstamped index (its first run) carries none.
	executor = &scriptedExecutor{rowResults: row("", 0)}
	repository, _ = newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	repository.WithIndexRegistry()
	if dispatch, err := repository.LoadPendingIndexIngest(context.Background(), "index-outbox-1"); err != nil ||
		dispatch.ExpectedEmbeddingModel != "" || dispatch.ExpectedEmbeddingDimension != 0 {
		t.Fatalf("unstamped dispatch = %+v err=%v", dispatch, err)
	}
}

func TestLoadPendingIndexIngestRejectsAmbiguousEmbeddingBindings(t *testing.T) {
	manifestDigest := runtimedomain.SHA256([]byte("index-manifest"))
	embeddingBindingDigest := runtimedomain.SHA256([]byte("embedding-binding"))
	deadline := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		"index-outbox-1", "index-command-1", "index-execution-1", int64(1), int64(1),
		"tenant-1", "1", "1", "actor-1",
		"index-bundle-1", "admission:index-bundle-1", "application/x-protobuf", int64(256), manifestDigest[:],
		"1", "indexing", "project", int32(1), deadline, "index-limits-v1", "", "",
		"toolkit-configuration", "tool-parameters", "llm-model", "llm-configuration", "mcp-credential-references",
		"embedding-binding", embeddingBindingDigest[:], int64(2),
		"stream-1", "message-1", "chat_predict", "user", "", int32(0),
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.LoadPendingIndexIngest(context.Background(), "index-outbox-1"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous embedding bindings") {
		t.Fatalf("ambiguous embedding binding error = %v", err)
	}
}

func TestStorePreparedIndexIngestReturnsCompetingCASWinner(t *testing.T) {
	candidate := repositoryPreparedEnvelope("index-candidate")
	winner := repositoryPreparedEnvelope("index-winner")
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		winner.Bytes, winner.Digest[:], winner.SignatureProfile, winner.KeyID,
		false, false, false, false, "PENDING",
	}}}}
	store := &scriptedStore{scriptedExecutor: executor}
	repository, err := newCommandOutboxRepository(store, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := repository.StorePreparedIndexIngest(context.Background(), "index-outbox-1", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Envelope.Digest != winner.Digest || string(selected.Envelope.Bytes) != string(winner.Bytes) || selected.Envelope.KeyID != winner.KeyID {
		t.Fatalf("repository did not return index CAS winner: %+v", selected)
	}
	query := executor.rowCalls[0].sql
	if !strings.Contains(query, "j.capability_id = 'index.ingest.v1'") ||
		!strings.Contains(query, "j.desired_state = 'RUNNING'") ||
		!strings.Contains(query, "i.index_meta_initialized_at IS NOT NULL") ||
		!strings.Contains(query, "FOR UPDATE OF j, o") ||
		strings.Contains(query, "configuration.validate.v1") {
		t.Fatal("prepared index CAS is not capability-scoped and locked")
	}
}

func TestStorePreparedIndexIngestDoesNotRedeliverWinnerAfterAuthority(t *testing.T) {
	winner := repositoryPreparedEnvelope("index-winner")
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		winner.Bytes, winner.Digest[:], winner.SignatureProfile, winner.KeyID,
		true, false, false, true, "RUNNING",
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.StorePreparedIndexIngest(
		context.Background(),
		"index-outbox-1",
		repositoryPreparedEnvelope("late-signer"),
	); !errors.Is(err, executionapp.ErrDispatchRetired) {
		t.Fatalf("late competing signer error=%v, want retired no-op", err)
	}
	if len(executor.execCalls) != 0 {
		t.Fatal("late competing signer mutated a command after authority")
	}
}

// TestStorePreparedIndexIngestReturnsWinnerAfterCompetingPublisherPublished
// pins the window that lost one publish attempt. This publisher signs while a
// competitor stores, appends and publishes the same command, so the job is
// DISPATCHED when this publisher reaches the envelope CAS. No worker holds
// authority yet, so the durable winner stays returnable. A refusal here is
// reported to the dispatcher as success: the publisher drops its append and
// its publish attempt without an error.
func TestStorePreparedIndexIngestReturnsWinnerAfterCompetingPublisherPublished(t *testing.T) {
	winner := repositoryPreparedEnvelope("index-winner")
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		winner.Bytes, winner.Digest[:], winner.SignatureProfile, winner.KeyID,
		true, false, false, false, "DISPATCHED",
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := repository.StorePreparedIndexIngest(
		context.Background(),
		"index-outbox-1",
		repositoryPreparedEnvelope("held-signer"),
	)
	if err != nil {
		t.Fatalf("held competing signer error = %v, want the durable winner", err)
	}
	if selected.Envelope.Digest != winner.Digest || string(selected.Envelope.Bytes) != string(winner.Bytes) || !selected.Published {
		t.Fatalf("held competing signer did not receive the published winner: %+v", selected)
	}
	if len(executor.execCalls) != 0 {
		t.Fatal("held competing signer replaced the durable winner")
	}
}

// TestStorePreparedIndexIngestRefusesNewEnvelopeAfterDispatch keeps the
// envelope CAS a one-shot selection. A dispatched job already selected its
// durable winner, so a candidate that finds no stored envelope must not store
// a second signature.
func TestStorePreparedIndexIngestRefusesNewEnvelopeAfterDispatch(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		nil, nil, int32(0), "",
		false, false, false, false, "DISPATCHED",
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.StorePreparedIndexIngest(
		context.Background(),
		"index-outbox-1",
		repositoryPreparedEnvelope("held-signer"),
	); !errors.Is(err, ErrPendingIndexIngestDispatchNotFound) {
		t.Fatalf("dispatched job without a stored winner error = %v, want not found", err)
	}
	if len(executor.execCalls) != 0 {
		t.Fatal("dispatched job stored a second index signature")
	}
}

func TestLoadPreparedIndexIngestDoesNotRedeliverAfterAuthority(t *testing.T) {
	winner := repositoryPreparedEnvelope("index-winner")
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
		winner.Bytes, winner.Digest[:], winner.SignatureProfile, winner.KeyID,
		true, false, false, true, "RUNNING",
	}}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.LoadPreparedIndexIngest(context.Background(), "index-outbox-1"); !errors.Is(err, executionapp.ErrDispatchRetired) {
		t.Fatalf("authorized prepared command error=%v, want retired no-op", err)
	}
	if !strings.Contains(executor.rowCalls[0].sql, "i.index_meta_initialized_at IS NOT NULL") ||
		!strings.Contains(executor.rowCalls[0].sql, "j.desired_state = 'RUNNING'") {
		t.Fatal("prepared-envelope reload bypasses metadata initialization or cancellation")
	}
}

func TestRetireNoAuthorityIndexIngestUsesBoundedCapabilityScans(t *testing.T) {
	executor := &scriptedExecutor{rowsResults: []*scriptedRows{{}, {}}}
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: executor}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	retired, err := repository.RetireNoAuthorityIndexIngest(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 || len(executor.queryCalls) != 2 {
		t.Fatalf("unexpected index retirement: retired=%d queries=%d", retired, len(executor.queryCalls))
	}
	for _, call := range executor.queryCalls {
		if !strings.Contains(call.sql, "j.capability_id = 'index.ingest.v1'") || !strings.Contains(call.sql, "o.stream_name = $1") || !strings.Contains(call.sql, "LIMIT $2") || !strings.Contains(call.sql, "SKIP LOCKED") || strings.Contains(call.sql, "configuration.validate.v1") {
			t.Fatal("index retirement scan escaped its capability, stream or bound")
		}
		if !reflect.DeepEqual(call.args, []any{"runtime:index:commands", int32(3)}) {
			t.Fatalf("unexpected index retirement args: %#v", call.args)
		}
	}
}

func TestRetireNoAuthorityIndexIngestPersistsTerminalIntentInSameTransaction(t *testing.T) {
	executor := &scriptedExecutor{
		rowsResults: []*scriptedRows{{rows: []scriptedRow{{values: []any{
			"index-outbox-1",
			"index-execution-1",
			int64(1),
			int64(42),
			"CANCELLED",
		}}}}},
		rowResults: []scriptedRow{{values: []any{int64(42)}}},
	}
	store := &scriptedStore{scriptedExecutor: executor}
	repository, err := newCommandOutboxRepository(store, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	activity := &recordingCurrentIndexActivityProjector{}
	repository.activity = activity
	retired, err := repository.RetireNoAuthorityIndexIngest(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 || store.txCalls != 1 ||
		len(executor.rowCalls) != 1 || len(executor.execCalls) != 2 {
		t.Fatalf(
			"retired=%d tx=%d rows=%d execs=%d",
			retired,
			store.txCalls,
			len(executor.rowCalls),
			len(executor.execCalls),
		)
	}
	intent := executor.execCalls[1]
	for _, predicate := range []string{
		"UPDATE elitea_runtime.index_ingest_jobs AS i",
		"FROM elitea_runtime.command_outbox AS o",
		"o.retired_at IS NOT NULL",
		"o.authority_granted_at IS NULL",
		"i.capability_id = 'index.ingest.v1'",
		"i.index_meta_initialized_at IS NOT NULL",
		"i.index_meta_terminal_status IS NULL",
		"index_meta_terminal_status = 'PENDING'",
	} {
		if !strings.Contains(intent.sql, predicate) {
			t.Fatalf("retirement intent SQL is missing %q", predicate)
		}
	}
	if !reflect.DeepEqual(intent.args, []any{
		"index-outbox-1",
		"index-execution-1",
		int64(1),
		"cancelled",
		retirementCodeCancelled,
	}) {
		t.Fatalf("retirement intent args=%#v", intent.args)
	}
	if len(activity.terminals) != 1 ||
		activity.terminals[0].ExecutionID != "index-execution-1" ||
		activity.terminals[0].Generation != 1 ||
		activity.terminals[0].Message != "Execution was cancelled." ||
		!activity.terminals[0].IsError {
		t.Fatalf("manual Stop did not terminalize current Activity: %+v", activity.terminals)
	}
}

func TestIndexIngestRepositoryRejectsInvalidBounds(t *testing.T) {
	repository, err := newCommandOutboxRepository(&scriptedStore{scriptedExecutor: &scriptedExecutor{}}, "runtime:index:commands")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ListPendingIndexIngestIDs(context.Background(), executionapp.MaxOutboxPublisherBatchSize+1, time.Minute); err != executionapp.ErrInvalidPendingOutboxLimit {
		t.Fatalf("invalid index list bound error = %v", err)
	}
	if _, err := repository.RetireNoAuthorityIndexIngest(context.Background(), 0); err != executionapp.ErrInvalidPendingOutboxLimit {
		t.Fatalf("invalid index retirement bound error = %v", err)
	}
}
