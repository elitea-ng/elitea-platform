package pipelineruns_test

// End-to-end acceptance for pipeline.run.succeeded/failed: a REAL
// public.pipeline_runs row (migrations/shared/0124), a REAL webhook.Dispatcher
// wired to a REAL webhook_deliveries log (migrations/shared/0122/0123), and
// a REAL httptest receiver — proving the settlement hook's payload actually
// reaches, and is signed for, a subscriber.
//
// This does NOT reconstruct the claim-fence/gRPC worker protocol
// (control.Server.PrepareSettlement's authenticated workload session,
// output.AgentExecutionService's fence verification, and so on) — that
// machinery is proven elsewhere (internal/application/execution,
// internal/transport/runtimegrpc/control and output's own test suites), and
// pipelineruns.NewSettlementHook's OWN contract only ever sees a
// SettlementProposal and SettlementReceipt that execution.SettlementService
// has ALREADY validated and committed by the time it calls a hook (see that
// package's PrepareSettlement — hooks run strictly after the repository call
// returns). So this test builds a hook the same way
// runtimecomposition.New wires one, and drives it directly with a
// proposal/receipt pair shaped exactly like a real settlement would produce,
// which is the precise contract the hook promises to honour.
//
// Skips (not fails) with no ELITEA_TEST_DATABASE_URL, same as every other
// _postgres_integration_test.go in this service.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/pipelineruns"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const pipelineRunsDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
const pipelineRunsBootstrapSchema = "../../infra/db/migrations/001_initial.sql"

var pipelineRunsTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(pipelineRunsDatabaseURLEnv)
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(pipelineRunsBootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files: platformmigrations.Files,
		Seed:  string(bootstrap),
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build pipelineruns template: %v\n", err)
		os.Exit(1)
	}
	pipelineRunsTemplate = templateName
	os.Exit(m.Run())
}

func newPipelineRunsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(pipelineRunsDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", pipelineRunsDatabaseURLEnv)
	}
	if pipelineRunsTemplate == "" {
		t.Fatalf("TestMain did not build the pipelineruns template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_pipelineruns_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, pipelineRunsTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", pipelineRunsDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

func waitForPipelineRunDeliveries(t *testing.T, deliveries webhook.DeliveryRepository, projectID, webhookID string, count int) []webhook.Delivery {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		items, err := deliveries.ListRecent(context.Background(), projectID, webhookID, 20)
		if err != nil {
			t.Fatalf("ListRecent: %v", err)
		}
		if len(items) >= count {
			return items
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("did not observe %d delivery row(s) before the deadline", count)
	return nil
}

func settledProposal(executionID string, outcome executionapp.SettlementOutcome) executionapp.SettlementProposal {
	return executionapp.SettlementProposal{
		Fence:                   runtimedomain.Fence{ExecutionID: executionID, Generation: 1},
		ProposalID:              "proposal-" + executionID,
		Outcome:                 outcome,
		TerminalLogicalOutputID: "agent-execution:" + executionID,
		TerminalEventID:         "event-" + executionID,
		TerminalSequence:        1,
	}
}

// TestPipelineRunSucceededReachesAWebhookReceiverSignedAndLogged is the
// success half: a tracked pipeline run whose settlement hook fires
// SUCCEEDED reaches a registered webhook as a signed pipeline.run.succeeded
// POST, and the delivery is logged.
func TestPipelineRunSucceededReachesAWebhookReceiverSignedAndLogged(t *testing.T) {
	pool := newPipelineRunsPool(t)
	ctx := context.Background()

	var received int32
	var gotSignature, gotEventType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		gotSignature = r.Header.Get(webhook.SignatureHeader)
		gotEventType = r.Header.Get(webhook.EventTypeHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo)
	domainEvents := events.NewPublisher(events.NoopBus{}, dispatcher)

	pipelineRunsRepo := repos.NewPipelineRunsRepo(pool)
	hook := pipelineruns.NewSettlementHook(pipelineRunsRepo, domainEvents)

	const projectID = "proj-pipeline-succeeded"
	registered, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL:    server.URL,
		Events: []string{"pipeline.run.succeeded"},
		Secret: "pipeline-secret",
		Active: true,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	const executionID = "exec-pipeline-succeeded"
	if err := pipelineRunsRepo.RecordRunStart(ctx, pipelineruns.Run{
		ExecutionID:      executionID,
		ProjectID:        projectID,
		ApplicationID:    42,
		VersionID:        3,
		ConversationUUID: "conv-pipeline-succeeded",
		Origin:           "Webhook",
	}); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	// The AfterSettle hook, called the way execution.SettlementService calls
	// it: after the (here, simulated) settlement has already committed.
	hook(ctx, settledProposal(executionID, executionapp.SettlementSucceeded), executionapp.SettlementReceipt{ID: "receipt-1", Outcome: executionapp.SettlementSucceeded})

	items := waitForPipelineRunDeliveries(t, deliveriesRepo, projectID, registered.ID, 1)
	if atomic.LoadInt32(&received) != 1 {
		t.Fatalf("receiver saw %d requests, want 1", received)
	}
	if gotSignature == "" {
		t.Fatal("delivery carried no signature header")
	}
	if gotEventType != "pipeline.run.succeeded" {
		t.Errorf("event type header = %q, want pipeline.run.succeeded", gotEventType)
	}

	delivery := items[0]
	if delivery.Status != webhook.DeliveryStatusSuccess {
		t.Errorf("status = %s, want success", delivery.Status)
	}
	if delivery.Event != "pipeline.run.succeeded" {
		t.Errorf("event = %s, want pipeline.run.succeeded", delivery.Event)
	}
}

// TestPipelineRunFailedCarriesTheErrorSummaryToTheReceiver is the failure
// half: a captured error message (the same path
// output.RuntimeFailureService's observer writes through, here written
// directly for the same reason the settlement side of this test drives the
// hook directly) reaches the receiver's payload.
func TestPipelineRunFailedCarriesTheErrorSummaryToTheReceiver(t *testing.T) {
	pool := newPipelineRunsPool(t)
	ctx := context.Background()

	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		gotBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo)
	domainEvents := events.NewPublisher(events.NoopBus{}, dispatcher)

	pipelineRunsRepo := repos.NewPipelineRunsRepo(pool)
	hook := pipelineruns.NewSettlementHook(pipelineRunsRepo, domainEvents)

	const projectID = "proj-pipeline-failed"
	registered, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL:    server.URL,
		Events: []string{"pipeline.run.failed"},
		Secret: "s",
		Active: true,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	const executionID = "exec-pipeline-failed"
	if err := pipelineRunsRepo.RecordRunStart(ctx, pipelineruns.Run{
		ExecutionID: executionID, ProjectID: projectID, ApplicationID: 1, VersionID: 1, ConversationUUID: "conv-pipeline-failed", Origin: "Schedule",
	}); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}
	// The error-text half — what output.RuntimeFailureService's observer
	// would have already written before settlement, per the package doc.
	if err := pipelineRunsRepo.RecordExecutionError(ctx, executionID, "the pipeline's third node exhausted its tool budget"); err != nil {
		t.Fatalf("RecordExecutionError: %v", err)
	}

	hook(ctx, settledProposal(executionID, executionapp.SettlementFailed), executionapp.SettlementReceipt{ID: "receipt-2", Outcome: executionapp.SettlementFailed})

	items := waitForPipelineRunDeliveries(t, deliveriesRepo, projectID, registered.ID, 1)
	delivery := items[0]
	if delivery.Event != "pipeline.run.failed" {
		t.Errorf("event = %s, want pipeline.run.failed", delivery.Event)
	}
	if delivery.Status != webhook.DeliveryStatusSuccess {
		t.Errorf("status = %s, want success (the DELIVERY succeeded; the pipeline run failed)", delivery.Status)
	}
	if len(gotBody) == 0 {
		t.Fatal("receiver saw no body")
	}
	bodyText := string(gotBody)
	if !strings.Contains(bodyText, "the pipeline's third node exhausted its tool budget") {
		t.Errorf("delivered body does not carry the error summary: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"status":"failed"`) {
		t.Errorf("delivered body does not carry status=failed: %s", bodyText)
	}
}

// TestPipelineRunOutcomeIsNotEmittedTwiceForAReplayedSettlement proves the
// exactly-once contract end to end: the hook is called twice for the same
// execution (a settlement replay), and only one webhook delivery results.
func TestPipelineRunOutcomeIsNotEmittedTwiceForAReplayedSettlement(t *testing.T) {
	pool := newPipelineRunsPool(t)
	ctx := context.Background()

	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhooksRepo := repos.NewWebhooksRepo(pool)
	deliveriesRepo := repos.NewWebhookDeliveriesRepo(pool)
	dispatcher := webhook.NewDispatcher(webhooksRepo, deliveriesRepo)
	domainEvents := events.NewPublisher(events.NoopBus{}, dispatcher)

	pipelineRunsRepo := repos.NewPipelineRunsRepo(pool)
	hook := pipelineruns.NewSettlementHook(pipelineRunsRepo, domainEvents)

	const projectID = "proj-pipeline-replay"
	registered, err := webhooksRepo.Create(ctx, projectID, webhook.Webhook{
		URL: server.URL, Events: []string{"pipeline.run.succeeded"}, Secret: "s", Active: true,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	const executionID = "exec-pipeline-replay"
	if err := pipelineRunsRepo.RecordRunStart(ctx, pipelineruns.Run{
		ExecutionID: executionID, ProjectID: projectID, ApplicationID: 1, VersionID: 1, ConversationUUID: "conv-replay", Origin: "Webhook",
	}); err != nil {
		t.Fatalf("RecordRunStart: %v", err)
	}

	proposal := settledProposal(executionID, executionapp.SettlementSucceeded)
	receipt := executionapp.SettlementReceipt{ID: "receipt-replay", Outcome: executionapp.SettlementSucceeded}
	hook(ctx, proposal, receipt)
	hook(ctx, proposal, receipt) // the replay

	waitForPipelineRunDeliveries(t, deliveriesRepo, projectID, registered.ID, 1)
	// Give a would-be second delivery a moment to have arrived if the
	// dedup guard were broken, then assert it did not.
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&received); got != 1 {
		t.Fatalf("receiver saw %d requests across a replayed settlement, want exactly 1", got)
	}
}
