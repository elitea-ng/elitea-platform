package repos

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type indexIngestReadQueriesStub struct {
	header       sqlcgen.GetExpectedIndexIngestHeaderRow
	headerErr    error
	entries      []sqlcgen.ListExpectedIndexIngestEntriesRow
	entriesErr   error
	artifact     sqlcgen.GetDurableIndexResultArtifactRow
	artifactErr  error
	headerCalls  int
	entryCalls   int
	artifactArgs []sqlcgen.GetDurableIndexResultArtifactParams
}

func (s *indexIngestReadQueriesStub) GetExpectedIndexIngestHeader(_ context.Context, _ sqlcgen.GetExpectedIndexIngestHeaderParams) (sqlcgen.GetExpectedIndexIngestHeaderRow, error) {
	s.headerCalls++
	return s.header, s.headerErr
}

func (s *indexIngestReadQueriesStub) ListExpectedIndexIngestEntries(_ context.Context, _ sqlcgen.ListExpectedIndexIngestEntriesParams) ([]sqlcgen.ListExpectedIndexIngestEntriesRow, error) {
	s.entryCalls++
	return append([]sqlcgen.ListExpectedIndexIngestEntriesRow(nil), s.entries...), s.entriesErr
}

func (s *indexIngestReadQueriesStub) GetDurableIndexResultArtifact(_ context.Context, params sqlcgen.GetDurableIndexResultArtifactParams) (sqlcgen.GetDurableIndexResultArtifactRow, error) {
	s.artifactArgs = append(s.artifactArgs, params)
	return s.artifact, s.artifactErr
}

func TestIndexIngestResultsRepositoryMapsSQLCBindingExactly(t *testing.T) {
	header, entries := storedIndexIngestFixture()
	queries := &indexIngestReadQueriesStub{header: header, entries: entries}
	repository := newTestIndexResultsRepository(t, queries, &scriptedExecutor{})

	expected, err := repository.ExpectedIndexIngest(context.Background(), header.ExecutionID, uint64(header.Generation))
	if err != nil {
		t.Fatal(err)
	}
	if queries.headerCalls != 1 || queries.entryCalls != 1 {
		t.Fatalf("SQLC binding lookup calls changed: header=%d entries=%d", queries.headerCalls, queries.entryCalls)
	}
	if expected.CapabilityID != executiondomain.IndexIngestCapability || expected.LogicalOutputID != "index-ingest:"+header.ExecutionID || expected.InputBundleID != header.InputBundleID || expected.InputBundleDigest != runtimedomain.SHA256([]byte("manifest")) {
		t.Fatalf("unexpected expected index identity: %+v", expected)
	}
	if expected.Bindings.ToolkitConfiguration.EntryID != "toolkit-configuration" || expected.Bindings.ToolParameters.EntryID != "tool-parameters" || !expected.Bindings.LLMModel.Present || expected.Bindings.LLMConfiguration.Present || !expected.Bindings.MCPTokens.Present || !expected.Bindings.EmbeddingBinding.Present {
		t.Fatalf("optional binding presence changed: %+v", expected.Bindings)
	}
	if expected.ArtifactContract.MediaType != "application/json" || expected.ArtifactContract.Classification != "project-confidential" || expected.ArtifactContract.MaxByteLength != 16*1024*1024 {
		t.Fatalf("unexpected artifact contract: %+v", expected.ArtifactContract)
	}
}

func TestIndexIngestResultsRepositoryRejectsUnboundMissingAndWrongRoleEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sqlcgen.GetExpectedIndexIngestHeaderRow, *[]sqlcgen.ListExpectedIndexIngestEntriesRow)
	}{
		{name: "unbound extra entry", mutate: func(_ *sqlcgen.GetExpectedIndexIngestHeaderRow, entries *[]sqlcgen.ListExpectedIndexIngestEntriesRow) {
			*entries = append(*entries, storedIndexEntry("unexpected", "unexpected.role", "extra"))
		}},
		{name: "missing required entry", mutate: func(_ *sqlcgen.GetExpectedIndexIngestHeaderRow, entries *[]sqlcgen.ListExpectedIndexIngestEntriesRow) {
			*entries = (*entries)[1:]
		}},
		{name: "wrong semantic role", mutate: func(_ *sqlcgen.GetExpectedIndexIngestHeaderRow, entries *[]sqlcgen.ListExpectedIndexIngestEntriesRow) {
			(*entries)[0].SemanticRole = executiondomain.IndexToolParametersRole
		}},
		{name: "unexpected optional absence", mutate: func(header *sqlcgen.GetExpectedIndexIngestHeaderRow, _ *[]sqlcgen.ListExpectedIndexIngestEntriesRow) {
			header.LlmModelEntryID = nil
		}},
		{name: "unexpected embedding absence", mutate: func(header *sqlcgen.GetExpectedIndexIngestHeaderRow, _ *[]sqlcgen.ListExpectedIndexIngestEntriesRow) {
			header.EmbeddingBindingEntryID = nil
		}},
		{name: "mixed classifications", mutate: func(_ *sqlcgen.GetExpectedIndexIngestHeaderRow, entries *[]sqlcgen.ListExpectedIndexIngestEntriesRow) {
			(*entries)[0].Classification = "public"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header, entries := storedIndexIngestFixture()
			test.mutate(&header, &entries)
			repository := newTestIndexResultsRepository(t, &indexIngestReadQueriesStub{header: header, entries: entries}, &scriptedExecutor{})
			if _, err := repository.ExpectedIndexIngest(context.Background(), header.ExecutionID, uint64(header.Generation)); err == nil {
				t.Fatal("corrupt stored input binding was accepted")
			}
		})
	}
}

func TestIndexIngestResultsRepositoryRejectsUnsupportedPersistedLimitsRevision(t *testing.T) {
	header, entries := storedIndexIngestFixture()
	header.LimitsRevision = "future-limits"
	queries := &indexIngestReadQueriesStub{header: header, entries: entries}
	repository := newTestIndexResultsRepository(t, queries, &scriptedExecutor{})
	if _, err := repository.ExpectedIndexIngest(context.Background(), header.ExecutionID, uint64(header.Generation)); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unsupported persisted limits revision was accepted: %v", err)
	}
	if queries.entryCalls != 0 {
		t.Fatal("unsupported limits revision reached entry materialization")
	}
}

func TestIndexIngestResultsRepositoryReturnsAuthoritativeDurableArtifact(t *testing.T) {
	frame, verified := testIndexIngestProjection(t)
	queries := &indexIngestReadQueriesStub{artifact: sqlcgen.GetDurableIndexResultArtifactRow{
		ArtifactID:       verified.Reference.ArtifactID,
		ImmutableVersion: verified.Reference.ImmutableVersion,
		MediaType:        verified.Reference.MediaType,
		ByteLength:       int64(verified.Reference.ByteLength),
		Digest:           append([]byte(nil), verified.Reference.Digest[:]...),
		Classification:   verified.Reference.Classification,
		StorageRecordID:  verified.StorageRecordID,
		BytesVerifiedAt:  pgtype.Timestamptz{Time: verified.VerifiedAt, Valid: true},
	}}
	repository := newTestIndexResultsRepository(t, queries, &scriptedExecutor{})
	request := outputapp.ArtifactVerificationRequest{
		TenantID:            frame.TenantID,
		ResourceProjectID:   frame.ResourceProjectID,
		ProjectionProjectID: frame.ProjectionProjectID,
		CommandID:           frame.Fence.CommandID,
		ExecutionID:         frame.Fence.ExecutionID,
		Generation:          frame.Fence.Generation,
		Artifact:            frame.Result.ResultArtifact,
	}

	actual, err := repository.VerifyDurable(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if actual != verified || len(queries.artifactArgs) != 1 || queries.artifactArgs[0].ArtifactID != request.Artifact.ArtifactID || queries.artifactArgs[0].ImmutableVersion != request.Artifact.ImmutableVersion || queries.artifactArgs[0].ResourceProjectID != 42 {
		t.Fatalf("durable artifact lookup lost identity: actual=%+v args=%+v", actual, queries.artifactArgs)
	}

	otherDigest := runtimedomain.SHA256([]byte("authoritative-other-bytes"))
	queries.artifact.Digest = append([]byte(nil), otherDigest[:]...)
	authoritative, err := repository.VerifyDurable(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if authoritative.Reference == request.Artifact {
		t.Fatal("verifier echoed forged worker metadata instead of authoritative storage metadata")
	}

	queries.artifactErr = pgx.ErrNoRows
	if _, err := repository.VerifyDurable(context.Background(), request); !errors.Is(err, outputapp.ErrIndexIngestArtifactUnavailable) {
		t.Fatalf("missing byte attestation was not reported as unavailable: %v", err)
	}
}

func TestIndexIngestResultsRepositoryProjectsAtomicallyAndReplaysIdempotently(t *testing.T) {
	frame, verified := testIndexIngestProjection(t)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{values: []any{"claim-index-1"}},
			{values: []any{"claim-index-1", "claim-index-1", "RUNNING", false, false, false}},
			{values: []any{frame.LogicalOutputID}},
			{values: []any{int64(41)}},
		},
		execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1")},
	}
	repository := newTestIndexResultsRepository(t, &indexIngestReadQueriesStub{}, executor)
	outcome, err := repository.ProjectIndexIngest(context.Background(), outputapp.IndexIngestProjection{Frame: frame, VerifiedArtifact: verified})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Inserted || outcome.Cursor != 41 || outcome.CommittedSequence != frame.Sequence {
		t.Fatalf("unexpected index projection outcome: %+v", outcome)
	}
	if len(executor.rowCalls) != 7 || len(executor.execCalls) != 1 {
		t.Fatalf("index projection escaped one atomic transaction script: rows=%d execs=%d", len(executor.rowCalls), len(executor.execCalls))
	}
	if executor.rowCalls[4].args[13] != payloadTypeIndexIngestResult || executor.rowCalls[4].args[17] != string(executionapp.SettlementSucceeded) {
		t.Fatalf("output inbox lost typed payload/settlement: %+v", executor.rowCalls[4].args)
	}
	if !strings.Contains(executor.rowCalls[5].sql, "index_ingest_results") || !strings.Contains(executor.rowCalls[5].sql, "index_result_artifacts") || executor.rowCalls[5].args[12] != verified.StorageRecordID {
		t.Fatal("index result was not bound to durable artifact metadata")
	}
	replayBytes, ok := executor.rowCalls[6].args[5].([]byte)
	if !ok || bytes.Contains(replayBytes, []byte(verified.StorageRecordID)) || executor.rowCalls[6].args[4] != replayEventIndexIngest {
		t.Fatal("browser replay event exposed storage identity or used the wrong event type")
	}

	record, _, err := indexOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	replayExecutor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: outputRecordScanValues(record)},
		{values: outputRecordScanValues(record)},
		{values: outputRecordScanValues(record)},
		{values: []any{int64(41)}},
	}}
	replayRepository := newTestIndexResultsRepository(t, &indexIngestReadQueriesStub{}, replayExecutor)
	replayed, err := replayRepository.ProjectIndexIngest(context.Background(), outputapp.IndexIngestProjection{Frame: frame, VerifiedArtifact: verified})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Inserted || replayed.Cursor != 41 || replayed.CommittedSequence != 1 || len(replayExecutor.rowCalls) != 4 || len(replayExecutor.execCalls) != 0 {
		t.Fatalf("identical index output replay was not idempotent: outcome=%+v rows=%d execs=%d", replayed, len(replayExecutor.rowCalls), len(replayExecutor.execCalls))
	}
}

func TestIndexIngestResultsRepositoryPersistsAndReplaysExactCurrentInlineSummary(t *testing.T) {
	frame, _ := testIndexIngestProjection(t)
	frame.Result.ResultArtifact = outputapp.IndexArtifactReference{}
	frame.Result.ResultSummary = outputapp.IndexIngestSummary{
		Status:         outputapp.IndexIngestStatusOK,
		Message:        "No new documents to index.",
		TerminalState:  outputapp.IndexIngestTerminalCompleted,
		Indexed:        7,
		Updated:        3,
		ReindexPresent: true,
		Reindex:        true,
	}
	frame.EncodedResult = []byte("deterministic-inline-index-summary")
	frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedResult)
	frame.Settlement.TerminalPayloadDigest = frame.PayloadDigest
	frame.EncodedSettlement = []byte("deterministic-inline-index-settlement")
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
	if err := frame.Validate(); err != nil {
		t.Fatalf("build inline summary projection: %v", err)
	}

	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{values: []any{"claim-index-1"}},
			{values: []any{"claim-index-1", "claim-index-1", "RUNNING", false, false, false}},
			{values: []any{frame.LogicalOutputID}},
			{values: []any{int64(42)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("INSERT 0 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}
	repository := newTestIndexResultsRepository(t, &indexIngestReadQueriesStub{}, executor)
	outcome, err := repository.ProjectIndexIngest(context.Background(), outputapp.IndexIngestProjection{Frame: frame})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Inserted || outcome.Cursor != 42 || len(executor.rowCalls) != 7 {
		t.Fatalf("unexpected inline projection: outcome=%+v row_calls=%d", outcome, len(executor.rowCalls))
	}
	if len(executor.execCalls) != 2 ||
		executor.execCalls[0].sql != "sqlc:InsertCurrentIndexTerminalNotification" ||
		executor.execCalls[0].args[2] != int64(7) ||
		executor.execCalls[0].args[3] != int64(3) ||
		executor.execCalls[0].args[4] != "completed" ||
		executor.execCalls[0].args[5] != "ok" ||
		executor.execCalls[0].args[6] != true ||
		executor.execCalls[0].args[7] != true {
		t.Fatalf("current terminal notification lost typed outcome: %+v", executor.execCalls)
	}
	projectionCall := executor.rowCalls[5]
	if !strings.Contains(projectionCall.sql, "completion_status, completion_message") || strings.Contains(projectionCall.sql, "index_result_artifacts") || projectionCall.args[5] != "ok" || projectionCall.args[6] != frame.Result.ResultSummary.Message {
		t.Fatalf("inline summary was not persisted as the closed typed shape: sql=%q args=%v", projectionCall.sql, projectionCall.args)
	}
	replayBytes, ok := executor.rowCalls[6].args[5].([]byte)
	wantReplay := []byte(`{"status":"ok","message":"No new documents to index."}`)
	if !ok || !bytes.Equal(replayBytes, wantReplay) {
		t.Fatalf("inline replay differs from current nested SDK result: got=%s want=%s", replayBytes, wantReplay)
	}
}

func TestIndexIngestResultsRepositoryConvergesFailedSummaryIntentAndReplay(t *testing.T) {
	frame, _ := testIndexIngestProjection(t)
	frame.Result.ResultArtifact = outputapp.IndexArtifactReference{}
	frame.Result.ResultSummary = outputapp.IndexIngestSummary{
		Status:        outputapp.IndexIngestStatusError,
		Message:       "Indexing failed before completion.",
		TerminalState: outputapp.IndexIngestTerminalFailed,
	}
	frame.Settlement.Outcome = executionapp.SettlementFailed
	frame.EncodedResult = []byte("deterministic-safe-index-failure")
	frame.PayloadDigest = runtimedomain.SHA256(frame.EncodedResult)
	frame.Settlement.TerminalPayloadDigest = frame.PayloadDigest
	frame.EncodedSettlement = []byte("deterministic-failed-index-settlement")
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(frame.EncodedSettlement)
	if err := frame.Validate(); err != nil {
		t.Fatalf("build failed summary projection: %v", err)
	}

	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{err: pgx.ErrNoRows},
			{values: []any{"claim-index-1"}},
			{values: []any{"claim-index-1", "claim-index-1", "RUNNING", false, false, false}},
			{values: []any{frame.LogicalOutputID}},
			{values: []any{int64(43)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 1"),
			pgconn.NewCommandTag("INSERT 0 1"),
			pgconn.NewCommandTag("UPDATE 1"),
		},
	}
	repository := newTestIndexResultsRepository(t, &indexIngestReadQueriesStub{}, executor)
	inserted, err := repository.ProjectIndexIngest(
		context.Background(),
		outputapp.IndexIngestProjection{Frame: frame},
	)
	if err != nil || !inserted.Inserted || inserted.Cursor != 43 {
		t.Fatalf("project failed index summary: outcome=%+v err=%v", inserted, err)
	}
	if len(executor.execCalls) != 3 ||
		executor.execCalls[0].args[2] != "failed" ||
		executor.execCalls[0].args[3] != frame.OccurredAt ||
		executor.execCalls[1].sql != "sqlc:InsertCurrentIndexTerminalNotification" ||
		executor.execCalls[1].args[1] != frame.Result.ResultSummary.Message ||
		executor.execCalls[1].args[4] != "failed" {
		t.Fatalf("failed PgVector terminal intent did not converge: calls=%+v", executor.execCalls)
	}
	replayBytes, ok := executor.rowCalls[6].args[5].([]byte)
	wantReplay := []byte(`{"status":"error","message":"Indexing failed before completion."}`)
	if !ok || !bytes.Equal(replayBytes, wantReplay) {
		t.Fatalf("failed UI replay changed: got=%s want=%s", replayBytes, wantReplay)
	}

	record, _, err := indexOutputRecord(frame)
	if err != nil {
		t.Fatal(err)
	}
	replayExecutor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: outputRecordScanValues(record)},
			{values: outputRecordScanValues(record)},
			{values: outputRecordScanValues(record)},
			{values: []any{int64(43)}},
		},
		execTags: []pgconn.CommandTag{
			pgconn.NewCommandTag("UPDATE 0"),
			pgconn.NewCommandTag("INSERT 0 0"),
		},
	}
	replayed, err := newTestIndexResultsRepository(
		t,
		&indexIngestReadQueriesStub{},
		replayExecutor,
	).ProjectIndexIngest(context.Background(), outputapp.IndexIngestProjection{Frame: frame})
	if err != nil || replayed.Inserted || replayed.Cursor != inserted.Cursor ||
		len(replayExecutor.execCalls) != 2 {
		t.Fatalf(
			"idempotent failed replay did not reconverge terminal intent: outcome=%+v calls=%d err=%v",
			replayed,
			len(replayExecutor.execCalls),
			err,
		)
	}
}

func newTestIndexResultsRepository(t *testing.T, queries indexIngestReadQueries, executor *scriptedExecutor) *IndexIngestResultsRepository {
	t.Helper()
	repository, err := newIndexIngestResultsRepository(queries, &scriptedProjectStore{scriptedExecutor: executor}, IndexIngestOutputPolicy{
		LimitsRevision:    "index-limits-v1",
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  16 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func storedIndexIngestFixture() (sqlcgen.GetExpectedIndexIngestHeaderRow, []sqlcgen.ListExpectedIndexIngestEntriesRow) {
	llmModel := "llm-model"
	mcpTokens := "mcp-credential-references"
	embeddingBinding := "embedding-binding"
	manifestDigest := runtimedomain.SHA256([]byte("manifest"))
	header := sqlcgen.GetExpectedIndexIngestHeaderRow{
		TenantID:                    "tenant-1",
		ResourceProjectID:           42,
		ProjectionProjectID:         42,
		CapabilityID:                executiondomain.IndexIngestCapability,
		CommandID:                   "command-index-1",
		ExecutionID:                 "execution-index-1",
		Generation:                  1,
		InputBundleID:               "input-bundle-index-1",
		InputBundleDigest:           append([]byte(nil), manifestDigest[:]...),
		ToolkitConfigurationEntryID: "toolkit-configuration",
		ToolParametersEntryID:       "tool-parameters",
		LlmModelEntryID:             &llmModel,
		McpTokensEntryID:            &mcpTokens,
		EmbeddingBindingEntryID:     &embeddingBinding,
		LimitsRevision:              "index-limits-v1",
	}
	entries := []sqlcgen.ListExpectedIndexIngestEntriesRow{
		storedIndexEntry("llm-model", executiondomain.IndexLLMModelRole, "llm"),
		storedIndexEntry("mcp-credential-references", executiondomain.IndexMCPTokensRole, "mcp"),
		storedIndexEntry("embedding-binding", executiondomain.IndexEmbeddingBindingRole, "embedding"),
		storedIndexEntry("tool-parameters", executiondomain.IndexToolParametersRole, "parameters"),
		storedIndexEntry("toolkit-configuration", executiondomain.IndexToolkitConfigurationRole, "toolkit"),
	}
	return header, entries
}

func storedIndexEntry(entryID, role, content string) sqlcgen.ListExpectedIndexIngestEntriesRow {
	digest := runtimedomain.SHA256([]byte(content))
	return sqlcgen.ListExpectedIndexIngestEntriesRow{
		EntryID:        entryID,
		EntryVersion:   digest.String(),
		SemanticRole:   role,
		ContentDigest:  append([]byte(nil), digest[:]...),
		Classification: "project-confidential",
	}
}

func testIndexIngestProjection(t *testing.T) (outputapp.IndexIngestFrame, outputapp.DurableIndexArtifact) {
	t.Helper()
	toolkitDigest := runtimedomain.SHA256([]byte("toolkit"))
	parametersDigest := runtimedomain.SHA256([]byte("parameters"))
	artifactBytes := []byte("artifact bytes are not carried by the output frame")
	artifactDigest := runtimedomain.SHA256(artifactBytes)
	fence := runtimedomain.Fence{
		CommandID:         "command-index-1",
		ExecutionID:       "execution-index-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/indexer/1",
		WorkloadSessionID: "session-index-1",
		ProducerID:        "producer-index-1",
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("index-fence-token"))),
	}
	artifact := outputapp.IndexArtifactReference{
		ArtifactID:       "artifact-index-1",
		ImmutableVersion: artifactDigest.String(),
		MediaType:        "application/json",
		ByteLength:       uint64(len(artifactBytes)),
		Digest:           artifactDigest,
		Classification:   "project-confidential",
	}
	payload := []byte("deterministic-index-result-protobuf")
	settlementBytes := []byte("deterministic-index-settlement-protobuf")
	payloadDigest := runtimedomain.SHA256(payload)
	logicalOutputID := "index-ingest:" + fence.ExecutionID
	frame := outputapp.IndexIngestFrame{
		StreamID:            fence.ExecutionID + ":1",
		TenantID:            "tenant-1",
		ResourceProjectID:   "42",
		ProjectionProjectID: "42",
		WorkloadSessionID:   fence.WorkloadSessionID,
		ProducerID:          fence.ProducerID,
		EventID:             fence.CommandID + ":1",
		LogicalOutputID:     logicalOutputID,
		Sequence:            1,
		OccurredAt:          time.Date(2026, time.July, 22, 15, 0, 0, 0, time.UTC),
		Fence:               fence,
		PayloadDigest:       payloadDigest,
		EncodedResult:       payload,
		Settlement: executionapp.SettlementProposal{
			Fence:                   fence,
			ProposalID:              fence.CommandID + ":settlement",
			Outcome:                 executionapp.SettlementSucceeded,
			TerminalLogicalOutputID: logicalOutputID,
			TerminalEventID:         fence.CommandID + ":1",
			TerminalSequence:        1,
			TerminalPayloadDigest:   payloadDigest,
			ProposalDigest:          runtimedomain.SHA256(settlementBytes),
			IdempotencyKey:          fence.CommandID + ":prepare-settlement",
		},
		EncodedSettlement: settlementBytes,
		Result: outputapp.IndexIngestResult{
			InputBundleID:     "input-bundle-index-1",
			InputBundleDigest: runtimedomain.SHA256([]byte("manifest")),
			Bindings: outputapp.IndexIngestBindings{
				ToolkitConfiguration: outputapp.IndexInputBinding{EntryID: "toolkit-configuration", ImmutableVersion: toolkitDigest.String(), ContentDigest: toolkitDigest},
				ToolParameters:       outputapp.IndexInputBinding{EntryID: "tool-parameters", ImmutableVersion: parametersDigest.String(), ContentDigest: parametersDigest},
			},
			ResultArtifact: artifact,
		},
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("build index projection frame: %v", err)
	}
	verified := outputapp.DurableIndexArtifact{
		Reference:       artifact,
		StorageRecordID: "storage-record-index-1",
		VerifiedAt:      time.Date(2026, time.July, 22, 15, 0, 1, 0, time.UTC),
	}
	return frame, verified
}
