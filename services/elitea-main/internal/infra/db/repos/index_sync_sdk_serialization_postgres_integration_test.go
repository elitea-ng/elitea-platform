package repos

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	indexSDKSerializationGateEnv       = "ELITEA_INDEX_SDK_SERIALIZATION_GATE"
	indexSDKSerializationContainerEnv  = "ELITEA_INDEX_SDK_CONTAINER"
	indexSDKSerializationContainer     = "centry-elitea-indexer-worker-1"
	indexSDKSerializationExpectedSHA   = "2caab1755e33356ab86d4d0c88a9087507f01ca57a9a7f1f00c166dc89a2fddd"
	indexSDKSerializationFirstTaskID   = "11111111111111111111111111111111"
	indexSDKSerializationSecondTaskID  = "22222222222222222222222222222222"
	indexSDKSerializationEventPrefix   = "ELITEA_SDK_SERIALIZATION_GATE "
	indexSDKSerializationProcessOutput = 64 << 10
)

var indexSDKSerializationContainerName = regexp.MustCompile(`\A[a-zA-Z0-9_.-]+\z`)

// TestPostgresPgvectorSameTargetSerializationAcrossInstalledSDKProcess is an
// opt-in release gate for the unchanged synchronous SDK. It crosses Main's
// production admission, claim, invocation-authority, Stop, output, settlement,
// terminal-effect and PgVector adapters against real PostgreSQL. A separate
// process inside the configured worker container calls the exact pinned
// EliteAClient.test_toolkit_tool entrypoint through EliteaSdkIndexingAdapter;
// its deterministic tool/provider-boundary fixture blocks that public SDK call
// without contacting a credentialed provider.
//
// command-bus reference delivery, the worker serve loop, gRPC, public authentication
// and a source provider are deliberately outside this gate. SETTLING is staged
// only after canonical output to represent the durable post-output/pre-receipt
// recovery window; it is not forced terminalization.
func TestPostgresPgvectorSameTargetSerializationAcrossInstalledSDKProcess(
	t *testing.T,
) {
	if os.Getenv(indexSDKSerializationGateEnv) != "1" {
		t.Skipf("set %s=1 to run the installed-SDK serialization gate", indexSDKSerializationGateEnv)
	}
	container := os.Getenv(indexSDKSerializationContainerEnv)
	if container == "" {
		container = indexSDKSerializationContainer
	}
	if !indexSDKSerializationContainerName.MatchString(container) {
		t.Fatalf("%s contains an invalid Docker container name", indexSDKSerializationContainerEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	requirePostgresSDKSerializationPrerequisites(t, ctx, container)
	pool := newMigratedPostgresIntegrationPool(t)
	requirePostgresVectorExtension(t, ctx, pool)

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
	toolkitID := int32(1_500_000_000 + time.Now().UnixNano()%500_000_000)
	indexName := "sdk-serialization"
	databaseURL := postgresIntegrationPoolURL(
		t,
		pool.Config().ConnString(),
		pool.Config().ConnConfig.Database,
	)
	materializedToolkit := postgresSDKSerializationToolkit(
		t,
		toolkitID,
		databaseURL,
	)
	initializer, err := indexingapp.NewCurrentIndexMetaInitializer(
		postgresFrozenToolkitClaimer{materialized: materializedToolkit},
		pgvector.NewCurrentIndexMetaWriter(),
	)
	if err != nil {
		t.Fatal(err)
	}

	firstRequest := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"sdk-serialization-first",
		indexName,
	)
	first, err := newPostgresSDKSerializationAdmissionService(
		t,
		jobs,
		"first",
		indexSDKSerializationFirstTaskID,
	).Submit(ctx, firstRequest)
	if err != nil || !first.Created {
		t.Fatalf("admit first logical generation: outcome=%+v err=%v", first, err)
	}
	materializePostgresSDKSerializationMeta(
		t,
		ctx,
		jobs,
		initializer,
		firstRequest,
		first,
	)

	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    policy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(ctx, first.ExecutionID, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	claimService := newPostgresSDKSerializationClaimService(t, pool)

	assertPostgresSDKSerializationJob(
		t,
		ctx,
		pool,
		first.ExecutionID,
		"CLAIMED",
		"RUNNING",
		"NOT_STARTED",
	)
	assertPostgresSDKSerializationConflict(
		t,
		ctx,
		jobs,
		toolkitID,
		indexName,
		"claimed",
		first.ExecutionID,
	)

	if disposition, beginErr := claimService.BeginExecution(ctx, fence); beginErr != nil ||
		disposition != executionapp.BeginExecutionStartedNow {
		t.Fatalf("begin first execution: disposition=%s err=%v", disposition, beginErr)
	}
	assertPostgresSDKSerializationJob(
		t,
		ctx,
		pool,
		first.ExecutionID,
		"RUNNING",
		"RUNNING",
		"PREPARING",
	)
	assertPostgresSDKSerializationConflict(
		t,
		ctx,
		jobs,
		toolkitID,
		indexName,
		"running-preparing",
		first.ExecutionID,
	)

	if disposition, authorizeErr := claimService.AuthorizeInvocation(ctx, fence); authorizeErr != nil ||
		disposition != executionapp.AuthorizeInvocationNow {
		t.Fatalf(
			"authorize first SDK invocation: disposition=%s err=%v",
			disposition,
			authorizeErr,
		)
	}
	assertPostgresSDKSerializationJob(
		t,
		ctx,
		pool,
		first.ExecutionID,
		"RUNNING",
		"RUNNING",
		"MAY_HAVE_STARTED",
	)

	sdkProcess := startBlockedInstalledSDK(t, ctx, container)
	entered := sdkProcess.waitFor(t, ctx, "entered")
	if entered.CallableModule != "elitea_sdk.runtime.clients.client" ||
		entered.CallableQualname != "EliteAClient.test_toolkit_tool" ||
		entered.SDKDigest != indexSDKSerializationExpectedSHA {
		t.Fatalf("unexpected installed SDK entry marker: %+v", entered)
	}

	cancellationRepository, err := NewCurrentIndexCancellationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	cancellations, err := indexingapp.NewCurrentIndexCancellationService(
		cancellationRepository,
	)
	if err != nil {
		t.Fatal(err)
	}
	transitioned, err := cancellations.Cancel(ctx, indexingapp.CurrentIndexCancelRequest{
		ProjectID:   1,
		ToolkitID:   int64(toolkitID),
		IndexName:   indexName,
		ExecutionID: first.ExecutionID,
	})
	if err != nil || !transitioned {
		t.Fatalf("record Stop during blocked SDK call: transitioned=%v err=%v", transitioned, err)
	}
	assertPostgresSDKSerializationJob(
		t,
		ctx,
		pool,
		first.ExecutionID,
		"RUNNING",
		"CANCELLED",
		"MAY_HAVE_STARTED",
	)
	assertPostgresSDKSerializationConflict(
		t,
		ctx,
		jobs,
		toolkitID,
		indexName,
		"stopped-may-have-started",
		first.ExecutionID,
	)

	completed := sdkProcess.releaseAndWait(t, ctx)
	if !completed.Success || completed.Status != "ok" {
		t.Fatalf("installed SDK callable did not complete normally: %+v", completed)
	}

	frame := postgresInlineIndexOutputFrame(t, expected, fence, outputapp.IndexIngestSummary{
		Status:  outputapp.IndexIngestStatusOK,
		Message: "No new documents to index.",
	})
	if _, err := newPostgresIndexOutputService(t, pool, results).IngestIndex(
		ctx,
		frame,
	); !errors.Is(err, outputapp.ErrOutputCancelled) {
		t.Fatalf("Stop did not win output linearization: %v", err)
	}
	stagePostgresSDKSerializationSettling(t, ctx, pool, first.ExecutionID)
	assertPostgresSDKSerializationJob(
		t,
		ctx,
		pool,
		first.ExecutionID,
		"SETTLING",
		"CANCELLED",
		"MAY_HAVE_STARTED",
	)
	assertPostgresSDKSerializationConflict(
		t,
		ctx,
		jobs,
		toolkitID,
		indexName,
		"settling",
		first.ExecutionID,
	)

	recovery := recoverPostgresSDKSerializationTerminal(
		t,
		ctx,
		pool,
		claimService,
		expected,
	)
	settlements, err := NewSettlementsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := settlements.PrepareSettlement(ctx, *recovery)
	if err != nil || receipt.Outcome != executionapp.SettlementCancelled {
		t.Fatalf("settle canonical cancellation: receipt=%+v err=%v", receipt, err)
	}
	assertPostgresSDKSerializationJob(
		t,
		ctx,
		pool,
		first.ExecutionID,
		"CANCELLED",
		"CANCELLED",
		"MAY_HAVE_STARTED",
	)

	terminalBindings, err := NewCurrentIndexMetaTerminalBindingsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	terminalClaims, err := terminalBindings.ClaimPendingTerminalEffects(
		ctx,
		"sdk-serialization-terminal",
		1,
		time.Minute,
	)
	if err != nil || len(terminalClaims) != 1 {
		t.Fatalf("claim canonical cancellation metadata effect: claims=%+v err=%v", terminalClaims, err)
	}
	oldTerminal := terminalClaims[0].CurrentIndexMetaTerminalRequest
	terminalizer, err := indexingapp.NewCurrentIndexMetaTerminalizer(
		terminalBindings,
		postgresFrozenToolkitClaimer{materialized: materializedToolkit},
		pgvector.NewCurrentIndexMetaWriter(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminalizer.Terminalize(ctx, oldTerminal); err != nil {
		t.Fatalf("terminalize first PgVector generation: %v", err)
	}
	if err := terminalBindings.ResolveTerminalEffect(
		ctx,
		terminalClaims[0],
		indexingapp.CurrentIndexMetaTerminalApplied,
	); err != nil {
		t.Fatalf("resolve first PgVector terminal effect: %v", err)
	}
	oldCreatedOn := postgresSDKSerializationCreatedOn(
		t,
		ctx,
		pool,
		toolkitID,
		indexName,
	)

	secondRequest := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"sdk-serialization-second",
		indexName,
	)
	second, err := newPostgresSDKSerializationAdmissionService(
		t,
		jobs,
		"second",
		indexSDKSerializationSecondTaskID,
	).Submit(ctx, secondRequest)
	if err != nil || !second.Created ||
		second.IndexGeneration != first.IndexGeneration+1 {
		t.Fatalf(
			"admit next logical generation: first=%+v second=%+v err=%v",
			first,
			second,
			err,
		)
	}
	materializePostgresSDKSerializationMeta(
		t,
		ctx,
		jobs,
		initializer,
		secondRequest,
		second,
	)

	if err := terminalizer.Terminalize(
		ctx,
		oldTerminal,
	); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("old terminal effect was not fenced by new generation: %v", err)
	}
	target := indexingapp.CurrentIndexMetaTarget{
		ConnectionString: databaseURL,
		SchemaID:         toolkitID,
	}
	if err := pgvector.NewCurrentIndexMetaWriter().MaterializeTaskID(
		ctx,
		target,
		indexingapp.CurrentTaskRestampIndexMeta{
			MetaID:          first.IndexMetaID,
			ExecutionID:     first.ExecutionID,
			Generation:      first.Generation,
			IndexGeneration: first.IndexGeneration,
			IndexName:       indexName,
			ToolkitID:       toolkitID,
			CreatedOn:       oldCreatedOn,
		},
	); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("old SDK task restamp was not fenced by new generation: %v", err)
	}
	assertPostgresSDKSerializationFinalMeta(
		t,
		ctx,
		pool,
		toolkitID,
		indexName,
		first,
		second,
	)
}

func requirePostgresSDKSerializationPrerequisites(
	t *testing.T,
	ctx context.Context,
	container string,
) {
	t.Helper()
	databaseURL := os.Getenv(postgresIntegrationDatabaseURL)
	if databaseURL == "" &&
		os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		t.Fatalf(
			"%s is required when %s=1",
			postgresIntegrationDatabaseURL,
			indexSDKSerializationGateEnv,
		)
	}
	docker, err := osexec.LookPath("docker")
	if err != nil {
		t.Fatalf("docker CLI is required when %s=1: %v", indexSDKSerializationGateEnv, err)
	}
	output := &boundedSDKGateBuffer{limit: indexSDKSerializationProcessOutput}
	command := osexec.CommandContext(
		ctx,
		docker,
		"inspect",
		"--format={{.State.Running}}",
		container,
	)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		t.Fatalf(
			"inspect configured SDK container %q: %v; output=%q",
			container,
			err,
			output.String(),
		)
	}
	if strings.TrimSpace(output.String()) != "true" {
		t.Fatalf("configured SDK container %q is not running", container)
	}
}

func requirePostgresVectorExtension(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	t.Helper()
	var available bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'vector'
)`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("the enabled SDK serialization gate requires the PostgreSQL vector extension")
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("install the required vector extension: %v", err)
	}
}

func postgresSDKSerializationToolkit(
	t *testing.T,
	toolkitID int32,
	databaseURL string,
) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"id":           toolkitID,
		"type":         "confluence",
		"toolkit_name": "confluence",
		"settings": map[string]any{
			"pgvector_configuration": map[string]any{
				"configuration_type":       "pgvector",
				"configuration_project_id": 1,
				"configuration_uuid":       "integration-pgvector",
				"connection_string":        databaseURL,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func newPostgresSDKSerializationAdmissionService(
	t *testing.T,
	repository *IndexIngestJobsRepository,
	prefix string,
	executionID string,
) *indexingapp.AdmissionService {
	t.Helper()
	factory, err := indexingapp.NewInputBundleFactory(indexingapp.InputProfile{
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, postgresIndexIDs(
		prefix+"-bundle",
		prefix+"-toolkit-content",
		prefix+"-parameters-content",
	))
	if err != nil {
		t.Fatal(err)
	}
	service, err := indexingapp.NewAdmissionService(
		repository,
		factory,
		time.Now,
		postgresIndexIDs(
			executionID,
			prefix+"-command",
			prefix+"-outbox",
			prefix+"-index-meta",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func materializePostgresSDKSerializationMeta(
	t *testing.T,
	ctx context.Context,
	jobs *IndexIngestJobsRepository,
	initializer *indexingapp.CurrentIndexMetaInitializer,
	request indexingapp.SubmitRequest,
	outcome indexingapp.AdmissionOutcome,
) {
	t.Helper()
	if err := initializer.MaterializeInitialIndexMeta(ctx, request, outcome); err != nil {
		t.Fatalf("materialize PgVector generation %d: %v", outcome.IndexGeneration, err)
	}
	if _, err := jobs.MarkIndexMetaInitialized(ctx, indexingapp.IndexMetaInitialization{
		ExecutionID:     outcome.ExecutionID,
		Generation:      outcome.Generation,
		IndexGeneration: outcome.IndexGeneration,
		MetaID:          outcome.IndexMetaID,
		CorrelationID:   outcome.IndexMetaCorrelationID,
	}); err != nil {
		t.Fatalf("open generation %d dispatch gate: %v", outcome.IndexGeneration, err)
	}
}

func newPostgresSDKSerializationClaimService(
	t *testing.T,
	pool *pgxpool.Pool,
) *executionapp.ClaimService {
	t.Helper()
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func assertPostgresSDKSerializationJob(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	executionID string,
	state string,
	desired string,
	invocation string,
) {
	t.Helper()
	var actualState, actualDesired, actualInvocation string
	if err := pool.QueryRow(ctx, `
SELECT state, desired_state, invocation_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1`,
		executionID,
	).Scan(&actualState, &actualDesired, &actualInvocation); err != nil {
		t.Fatal(err)
	}
	if actualState != state || actualDesired != desired || actualInvocation != invocation {
		t.Fatalf(
			"execution %s state=(%s,%s,%s), want=(%s,%s,%s)",
			executionID,
			actualState,
			actualDesired,
			actualInvocation,
			state,
			desired,
			invocation,
		)
	}
}

func assertPostgresSDKSerializationConflict(
	t *testing.T,
	ctx context.Context,
	jobs *IndexIngestJobsRepository,
	toolkitID int32,
	indexName string,
	phase string,
	activeExecutionID string,
) {
	t.Helper()
	request := postgresIndexMetaConvergenceRequest(
		toolkitID,
		"sdk-serialization-conflict-"+phase,
		indexName,
	)
	_, err := newPostgresSDKSerializationAdmissionService(
		t,
		jobs,
		"conflict-"+phase,
		"33333333333333333333333333333333",
	).Submit(ctx, request)
	if !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("%s same-target admission error=%v", phase, err)
	}
	var conflict *indexingapp.ActiveIndexConflictError
	if !errors.As(err, &conflict) || conflict.TaskID != activeExecutionID {
		t.Fatalf(
			"%s active conflict=%+v want task_id=%s",
			phase,
			conflict,
			activeExecutionID,
		)
	}
}

func stagePostgresSDKSerializationSettling(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	executionID string,
) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs AS j
SET state = 'SETTLING'
WHERE j.execution_id = $1
  AND j.generation = 1
  AND j.state = 'RUNNING'
  AND j.desired_state = 'CANCELLED'
  AND j.invocation_state = 'MAY_HAVE_STARTED'
  AND EXISTS (
      SELECT 1
      FROM elitea_runtime.output_inbox AS o
      WHERE o.execution_id = j.execution_id
        AND o.generation = j.generation
        AND o.projected_at IS NOT NULL
        AND o.settlement_outcome = 'CANCELLED'
  )`, executionID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("stage SETTLING recovery window affected %d rows", tag.RowsAffected())
	}
}

func recoverPostgresSDKSerializationTerminal(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	claims *executionapp.ClaimService,
	expected outputapp.ExpectedIndexIngest,
) *executionapp.SettlementProposal {
	t.Helper()
	var outboxID string
	if err := pool.QueryRow(ctx, `
SELECT outbox_id
FROM elitea_runtime.command_outbox
WHERE execution_id = $1 AND generation = $2`,
		expected.ExecutionID,
		int64(expected.Generation),
	).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	decision, err := claims.Claim(ctx, executionapp.ClaimRequest{
		CommandID:            expected.CommandID,
		OutboxID:             outboxID,
		ExecutionID:          expected.ExecutionID,
		Generation:           expected.Generation,
		CapabilityID:         executiondomain.IndexIngestCapability,
		SignedEnvelopeDigest: runtimedomain.SHA256([]byte("signed-index-command:" + expected.CommandID)),
		WorkloadIdentity:     "spiffe://elitea.test/index-worker/1",
		WorkloadSessionID:    "index-worker-session-1",
		ProducerID:           "index-worker-1",
	})
	if err != nil ||
		decision.Disposition != executionapp.ClaimRecoverTerminalACK ||
		decision.SettlementRecovery == nil ||
		decision.SettlementRecovery.Proposal == nil {
		t.Fatalf("recover canonical cancellation proposal: decision=%+v err=%v", decision, err)
	}
	return decision.SettlementRecovery.Proposal
}

func postgresSDKSerializationCreatedOn(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	toolkitID int32,
	indexName string,
) float64 {
	t.Helper()
	schema := pgx.Identifier{strconv.FormatInt(int64(toolkitID), 10)}.Sanitize()
	var createdOn float64
	if err := pool.QueryRow(ctx, `
SELECT (cmetadata->>'created_on')::double precision
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata->>'collection' = $1
  AND cmetadata->>'type' = 'index_meta'`,
		indexName,
	).Scan(&createdOn); err != nil {
		t.Fatal(err)
	}
	return createdOn
}

func assertPostgresSDKSerializationFinalMeta(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	toolkitID int32,
	indexName string,
	first indexingapp.AdmissionOutcome,
	second indexingapp.AdmissionOutcome,
) {
	t.Helper()
	schema := pgx.Identifier{strconv.FormatInt(int64(toolkitID), 10)}.Sanitize()
	var raw []byte
	if err := pool.QueryRow(ctx, `
SELECT cmetadata
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata->>'collection' = $1
  AND cmetadata->>'type' = 'index_meta'`,
		indexName,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	historyEncoded, ok := metadata["history"].(string)
	if !ok {
		t.Fatalf("final history type=%T", metadata["history"])
	}
	var history []map[string]any
	if err := json.Unmarshal([]byte(historyEncoded), &history); err != nil {
		t.Fatal(err)
	}
	if metadata["index_meta_id"] != second.IndexMetaID ||
		metadata["execution_id"] != second.ExecutionID ||
		metadata["index_generation"] != float64(second.IndexGeneration) ||
		metadata["task_id"] != second.ExecutionID ||
		metadata["state"] != "in_progress" ||
		len(history) != 2 {
		t.Fatalf("final metadata=%#v history=%#v", metadata, history)
	}
	old := history[1]
	if old["index_meta_id"] != first.IndexMetaID ||
		old["execution_id"] != first.ExecutionID ||
		old["index_generation"] != float64(first.IndexGeneration) ||
		old["task_id"] != nil ||
		old["state"] != "cancelled" {
		t.Fatalf("old history generation was overwritten: %#v", history)
	}
}

type installedSDKGateEvent struct {
	Phase            string `json:"phase"`
	CallableModule   string `json:"callable_module"`
	CallableQualname string `json:"callable_qualname"`
	SDKDigest        string `json:"sdk_digest"`
	Success          bool   `json:"success"`
	Status           string `json:"status"`
}

type installedSDKGateProcess struct {
	command *osexec.Cmd
	stdin   io.WriteCloser
	events  <-chan installedSDKGateEvent
	scanErr <-chan error
	stderr  *boundedSDKGateBuffer
	done    bool
}

func startBlockedInstalledSDK(
	t *testing.T,
	ctx context.Context,
	container string,
) *installedSDKGateProcess {
	t.Helper()
	command := osexec.CommandContext(
		ctx,
		"docker",
		"exec",
		"-i",
		container,
		"python",
		"-c",
		blockedInstalledSDKScript,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &boundedSDKGateBuffer{limit: indexSDKSerializationProcessOutput}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start installed SDK process in %s: %v", container, err)
	}
	eventChannel := make(chan installedSDKGateEvent, 4)
	errorChannel := make(chan error, 1)
	go func() {
		defer close(eventChannel)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), indexSDKSerializationProcessOutput)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, indexSDKSerializationEventPrefix) {
				continue
			}
			var event installedSDKGateEvent
			if err := json.Unmarshal(
				[]byte(strings.TrimPrefix(line, indexSDKSerializationEventPrefix)),
				&event,
			); err != nil {
				errorChannel <- fmt.Errorf("decode installed SDK marker: %w", err)
				return
			}
			eventChannel <- event
		}
		if err := scanner.Err(); err != nil {
			errorChannel <- err
		}
	}()
	process := &installedSDKGateProcess{
		command: command,
		stdin:   stdin,
		events:  eventChannel,
		scanErr: errorChannel,
		stderr:  stderr,
	}
	t.Cleanup(func() {
		if process.done {
			return
		}
		_ = process.stdin.Close()
		if process.command.Process != nil {
			_ = process.command.Process.Kill()
		}
		_ = process.command.Wait()
	})
	return process
}

func (p *installedSDKGateProcess) waitFor(
	t *testing.T,
	ctx context.Context,
	phase string,
) installedSDKGateEvent {
	t.Helper()
	for {
		select {
		case event, ok := <-p.events:
			if !ok {
				t.Fatalf(
					"installed SDK process ended before %s; stderr=%q",
					phase,
					p.stderr.String(),
				)
			}
			if event.Phase == phase {
				return event
			}
		case err := <-p.scanErr:
			t.Fatalf(
				"installed SDK output ended before %s: %v; stderr=%q",
				phase,
				err,
				p.stderr.String(),
			)
		case <-ctx.Done():
			t.Fatalf(
				"installed SDK process did not reach %s: %v; stderr=%q",
				phase,
				ctx.Err(),
				p.stderr.String(),
			)
		}
	}
}

func (p *installedSDKGateProcess) releaseAndWait(
	t *testing.T,
	ctx context.Context,
) installedSDKGateEvent {
	t.Helper()
	if _, err := io.WriteString(p.stdin, "release\n"); err != nil {
		t.Fatalf("release installed SDK callable: %v", err)
	}
	if err := p.stdin.Close(); err != nil {
		t.Fatalf("close installed SDK stdin: %v", err)
	}
	completed := p.waitFor(t, ctx, "completed")
	if err := p.command.Wait(); err != nil {
		t.Fatalf("installed SDK process failed: %v; stderr=%q", err, p.stderr.String())
	}
	p.done = true
	return completed
}

type boundedSDKGateBuffer struct {
	buffer bytes.Buffer
	limit  int
	mutex  sync.Mutex
}

func (b *boundedSDKGateBuffer) Write(value []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	accepted := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buffer.Write(value)
	}
	return accepted, nil
}

func (b *boundedSDKGateBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}

const blockedInstalledSDKScript = `
import json
import sys
import types

from elitea_sdk.runtime.clients.client import EliteAClient
from elitea_sdk.runtime.utils import toolkit_utils
from elitea_worker.agents.sdk_adapter import EliteaSdkIndexingAdapter
from elitea_worker.constants import SDK_PACKAGE_TREE_SHA256

PREFIX = "ELITEA_SDK_SERIALIZATION_GATE "


class BlockingTool:
    name = "index_data"

    def invoke(self, params, config=None):
        print(PREFIX + json.dumps({
            "phase": "entered",
            "callable_module": EliteAClient.test_toolkit_tool.__module__,
            "callable_qualname": EliteAClient.test_toolkit_tool.__qualname__,
            "sdk_digest": SDK_PACKAGE_TREE_SHA256,
        }, sort_keys=True), flush=True)
        if sys.stdin.readline().strip() != "release":
            raise RuntimeError("release token was not received")
        return {"status": "ok", "message": "No new documents to index."}


client = EliteAClient(
    project_id=1,
    base_url="https://unused.invalid",
    auth_token="system-test",
)
client._validate_toolkit_config = types.MethodType(
    lambda self, toolkit_config: toolkit_config,
    client,
)
client.get_llm = types.MethodType(
    lambda self, model, config: object(),
    client,
)
toolkit_utils.instantiate_toolkit_with_client = (
    lambda *args, **kwargs: [BlockingTool()]
)
adapter = EliteaSdkIndexingAdapter(client)
result = adapter.ingest(
    toolkit_config={
        "id": 1,
        "type": "confluence",
        "toolkit_name": "confluence",
        "settings": {},
    },
    tool_params={"index_name": "sdk-serialization"},
    runtime_config={},
    llm_model="system-test",
    llm_config={},
    mcp_tokens=None,
)
nested = result.get("result")
print(PREFIX + json.dumps({
    "phase": "completed",
    "callable_module": EliteAClient.test_toolkit_tool.__module__,
    "callable_qualname": EliteAClient.test_toolkit_tool.__qualname__,
    "sdk_digest": SDK_PACKAGE_TREE_SHA256,
    "success": result.get("success") is True,
    "status": nested.get("status") if isinstance(nested, dict) else "",
}, sort_keys=True), flush=True)
`
