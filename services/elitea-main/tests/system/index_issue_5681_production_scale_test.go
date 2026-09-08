package system_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimegrpc "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/nodeevent"
	"google.golang.org/protobuf/proto"
)

const (
	index5681OptIn                       = "ELITEA_INDEX_5681_SYSTEM_TEST"
	index5681CleanupOnly                 = "ELITEA_INDEX_5681_CLEANUP_ONLY"
	index5681CleanupManifestEnv          = "ELITEA_INDEX_5681_CLEANUP_MANIFEST"
	index5681FixturePortEnv              = "ELITEA_INDEX_5681_FIXTURE_PORT"
	index5681PythonEnv                   = "ELITEA_INDEX_5681_PYTHON"
	index5681DeniedCookieFileEnv         = "ELITEA_INDEX_5681_DENIED_COOKIE_FILE"
	index5681SecondProjectEnv            = "ELITEA_INDEX_5681_SECOND_PROJECT_ID"
	index5681SecondToolkitEnv            = "ELITEA_INDEX_5681_SECOND_TOOLKIT_ID"
	index5681WorkloadIdentityEnv         = "ELITEA_INDEX_5681_WORKLOAD_IDENTITY"
	index5681SourceAuthDigestEnv         = "ELITEA_INDEX_5681_SOURCE_AUTH_SHA256"
	index5681ModelAuthDigestEnv          = "ELITEA_INDEX_5681_MODEL_AUTH_SHA256"
	index5681ProxyAttestationEnv         = "ELITEA_INDEX_5681_LITELLM_ATTESTATION_SHA256"
	index5681SourceCanaryEnv             = "ELITEA_INDEX_5681_SOURCE_CREDENTIAL_CANARY"
	index5681ModelCanaryEnv              = "ELITEA_INDEX_5681_MODEL_CREDENTIAL_CANARY"
	index5681ProxyCanaryEnv              = "ELITEA_INDEX_5681_PROXY_CREDENTIAL_CANARY"
	index5681PlatformSHAEnv              = "ELITEA_INDEX_5681_PLATFORM_SHA"
	index5681MainImageIDEnv              = "ELITEA_INDEX_5681_MAIN_IMAGE_ID"
	index5681WorkerImageIDEnv            = "ELITEA_INDEX_5681_WORKER_IMAGE_ID"
	index5681LiteLLMImageIDEnv           = "ELITEA_INDEX_5681_LITELLM_IMAGE_ID"
	index5681GatewayImageIDEnv           = "ELITEA_INDEX_5681_GATEWAY_IMAGE_ID"
	index5681GatewayRouteDigestEnv       = "ELITEA_INDEX_5681_GATEWAY_ROUTE_SHA256"
	index5681GatewayBaseDigestEnv        = "ELITEA_INDEX_5681_GATEWAY_BASE_SHA256"
	index5681LiteLLMServiceEnv           = "ELITEA_INDEX_5681_LITELLM_SERVICE"
	index5681LiteLLMRevisionEnv          = "ELITEA_INDEX_5681_LITELLM_REVISION"
	index5681SDKRevisionEnv              = "ELITEA_INDEX_5681_SDK_REVISION"
	index5681ReceiptSchema               = "elitea.issue-5681.fixture-receipt.v1"
	index5681FixtureProfile              = "elitea.issue-5681.confluence-images.v1"
	index5681FixtureBytes          int64 = 62 << 20
	index5681LargeImageBytes             = 32 << 20
	index5681CurrentSourceBytes          = 2 * index5681FixtureBytes
	index5681CurrentVisionCalls          = 22
)

type index5681FixtureReceipt struct {
	Schema                     string         `json:"schema"`
	Profile                    string         `json:"profile"`
	DeclaredSourcePayloadBytes int64          `json:"declared_source_payload_bytes"`
	SmallImageSHA256           string         `json:"small_image_sha256"`
	LargeImageSHA256           string         `json:"large_image_sha256"`
	SourceCompletedRequests    map[string]int `json:"source_completed_requests"`
	SourceCompletedBytes       int64          `json:"source_completed_bytes"`
	ChatRequests               int            `json:"chat_requests"`
	MaxChatRequestBytes        int64          `json:"max_chat_request_bytes"`
	EmbeddingRequests          int            `json:"embedding_requests"`
	MaxEmbeddingRequestBytes   int64          `json:"max_embedding_request_bytes"`
	RejectedSourceRequests     int            `json:"rejected_source_requests"`
	RejectedModelRequests      int            `json:"rejected_model_requests"`
	RejectedProxyRequests      int            `json:"rejected_proxy_requests"`
}

type index5681Environment struct {
	deniedCookie             string
	secondProjectID          int64
	secondToolkitID          int64
	expectedWorkloadIdentity string
	sourceAuthDigest         string
	modelAuthDigest          string
	proxyAttestationDigest   string
	sourceCredentialCanary   string
	modelCredentialCanary    string
	proxyCredentialCanary    string
	platformSHA              string
	mainImageID              string
	workerImageID            string
	liteLLMImageID           string
	gatewayImageID           string
	gatewayRouteDigest       string
	gatewayBaseDigest        string
	liteLLMService           string
	liteLLMRevision          string
	sdkRevision              string
}

type index5681DurableSnapshot struct {
	State                   string `json:"state"`
	DesiredState            string `json:"desired_state"`
	InvocationState         string `json:"invocation_state"`
	TenantID                string `json:"tenant_id"`
	ResourceProjectID       int64  `json:"resource_project_id"`
	ProjectionProjectID     int64  `json:"projection_project_id"`
	WorkloadIdentity        string `json:"workload_identity"`
	WorkloadSessionID       string `json:"workload_session_id"`
	Claims                  int64  `json:"claims"`
	MaxClaimAttempt         int64  `json:"max_claim_attempt"`
	Results                 int64  `json:"results"`
	Settlements             int64  `json:"settlements"`
	ReplayEvents            int64  `json:"replay_events"`
	InputManifestBytes      int64  `json:"input_manifest_bytes"`
	InputEntryCount         int64  `json:"input_entry_count"`
	InputEntryBytes         int64  `json:"input_entry_bytes"`
	MaxInputEntryBytes      int64  `json:"max_input_entry_bytes"`
	PreparedEnvelopeBytes   int64  `json:"prepared_envelope_bytes"`
	MaxReplayEventBytes     int64  `json:"max_replay_event_bytes"`
	TotalReplayEventBytes   int64  `json:"total_replay_event_bytes"`
	NodeReplayEvents        int64  `json:"node_replay_events"`
	MaxOutputInboxBytes     int64  `json:"max_output_inbox_bytes"`
	TotalOutputInboxBytes   int64  `json:"total_output_inbox_bytes"`
	OutputInboxFrames       int64  `json:"output_inbox_frames"`
	LastNodeSequence        int64  `json:"last_node_sequence"`
	TerminalSequence        int64  `json:"terminal_sequence"`
	OutputIdentityBound     bool   `json:"output_identity_bound"`
	WorkloadSessionActive   bool   `json:"workload_session_active"`
	CompletionStatus        string `json:"completion_status"`
	CompletionMessage       string `json:"completion_message"`
	Published               bool   `json:"published"`
	AuthorityGranted        bool   `json:"authority_granted"`
	Retired                 bool   `json:"retired"`
	CommittedSettlement     bool   `json:"committed_settlement"`
	ReleasedClaims          int64  `json:"released_claims"`
	IndexCompletedReplay    int64  `json:"index_completed_replay"`
	TerminalReplayEventSeen bool   `json:"terminal_replay_event_seen"`
}

type index5681SSEObservation struct {
	EventCount         int
	EventIDs           []int64
	EventDigests       []string
	EventTypes         []string
	NodeTypes          []string
	ThinkingMessages   []string
	ThinkingToolNames  []string
	ThinkingToolkits   []string
	StatusStates       []string
	StatusIndexNames   []string
	StatusIndexed      []int64
	StatusUpdated      []int64
	StatusMetaID       string
	ToolRunID          string
	ToolStartedAt      string
	ToolkitDisplayName string
	MaxDataBytes       int
	TotalDataBytes     int
	ThinkingEvents     int
	TerminalEvents     int
	TerminalSeen       bool
	TerminalStatus     string
	TerminalMessage    string
	ThinkingIndex      int
	InProgressIndex    int
	CompletedIndex     int
	InvalidContract    bool
	UnsafeDataObserved bool
}

type index5681CommandEntryReference struct {
	EntryID          string
	SemanticRole     string
	ImmutableVersion string
	DigestHex        string
}

type index5681DecodedCommandEvidence struct {
	BundleID        string
	BundleVersion   string
	BundleDigestHex string
	BundleBytes     int64
	BundleMediaType string
	Entries         []index5681CommandEntryReference
}

type index5681RedisEvidence struct {
	EntryID           string `json:"entry_id"`
	FieldName         string `json:"field_name"`
	EnvelopeHex       string `json:"envelope_hex"`
	MappingField      string `json:"mapping_field"`
	MappingValue      string `json:"mapping_value"`
	StreamLength      int64  `json:"stream_length"`
	MappingCount      int64  `json:"mapping_count"`
	FieldCount        int64  `json:"field_count"`
	EnvelopeBytes     int64  `json:"envelope_bytes"`
	MappingFieldBytes int64  `json:"mapping_field_bytes"`
	MappingValueBytes int64  `json:"mapping_value_bytes"`
}

type index5681FixtureProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
	mu      sync.Mutex
	once    sync.Once
}

type index5681CleanupResource struct {
	ExecutionID string `json:"execution_id"`
	ProjectID   int64  `json:"project_id"`
	ToolkitID   int64  `json:"toolkit_id"`
	IndexName   string `json:"index_name"`
}

// TestExistingComposeIndexIssue5681ProductionScale is the opt-in, real-process
// acceptance gate for the production incident. The 62 MiB corpus stays on the
// Confluence/LiteLLM HTTP data plane. PostgreSQL owns only the bounded protected
// configuration bundle and Redis carries exactly one signed reference.
func TestExistingComposeIndexIssue5681ProductionScale(t *testing.T) {
	if os.Getenv(index5681OptIn) != "1" {
		t.Skip("run the fail-fast index_5681/run.sh wrapper to execute the production-scale gate")
	}
	if os.Getenv(index5681CleanupOnly) == "1" {
		t.Skip("cleanup-only owner process")
	}

	config := loadIndexReliabilityConfig(t)
	environment := loadIssue5681Environment(t, config)
	credentialCanaries := []string{
		environment.sourceCredentialCanary,
		environment.modelCredentialCanary,
		environment.proxyCredentialCanary,
	}
	requireIssue5681RequestProfile(t, config.startBody)
	harness := &indexComposeHarness{config: config}
	ctx, cancel := context.WithTimeout(context.Background(), config.timeout)
	defer cancel()

	fixturePort := requiredIssue5681Port(t)
	requireIssue5681SUTProvenance(t, ctx, harness, environment)
	fixture := startIssue5681Fixture(
		t,
		ctx,
		config.projectID,
		fixturePort,
		environment,
	)
	t.Cleanup(func() { fixture.stop(t) })
	fixtureBaseURL := fmt.Sprintf("http://127.0.0.1:%d", fixturePort)
	waitForIssue5681Fixture(t, ctx, fixtureBaseURL, fixture)
	initialReceipt := readIssue5681Receipt(t, ctx, fixtureBaseURL)

	requireIssue5681CleanDedicatedBaseline(t, ctx, harness)
	requireIssue5681RedisNoEviction(t, ctx, harness)
	t.Log("bounded PoV restriction: no adversarial recovery claimant and no preexisting Redis state/corruption; recovery-purpose enforcement and full Redis bijection are excluded")
	requireIssue5681ConfluenceToolkit(t, ctx, harness)
	requireIssue5681SecondProjectAccess(
		t,
		ctx,
		harness,
		environment.secondProjectID,
		environment.secondToolkitID,
	)
	requireWorkerCanReachIssue5681Fixture(t, ctx, harness, fixturePort)

	type admittedExecution struct {
		id        string
		indexName string
	}
	var admittedMu sync.Mutex
	var admitted []admittedExecution
	var indexNames []string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		admittedMu.Lock()
		defer admittedMu.Unlock()
		for _, execution := range admitted {
			_ = harness.stopIndex(cleanupCtx, execution.id, execution.indexName)
		}
		for _, createdIndexName := range indexNames {
			if err := cleanupIssue5681Index(
				cleanupCtx,
				harness,
				createdIndexName,
			); err != nil {
				t.Errorf("clean issue #5681 PgVector index: %v", err)
			}
		}
		_, _ = harness.compose(cleanupCtx, "start", indexWorkerService)
	})

	nonce := randomIndexReliabilityNonce(t)
	indexName := "rel-5681-" + nonce
	controlCanary := "issue-5681-control-canary-" + nonce
	startBody := prepareIndexReliabilityRequest(
		t,
		config.startBody,
		indexName,
		controlCanary,
	)
	indexNames = append(indexNames, indexName)

	harness.stopWorker(t, ctx)
	assertIssue5681AdmissionRBAC(
		t,
		ctx,
		harness,
		startBody,
		environment.deniedCookie,
	)
	executionID := harness.startIndex(t, ctx, startBody)
	appendIssue5681CleanupResource(t, index5681CleanupResource{
		ExecutionID: executionID,
		ProjectID:   config.projectID,
		ToolkitID:   config.toolkitID,
		IndexName:   indexName,
	})
	admittedMu.Lock()
	admitted = append(admitted, admittedExecution{id: executionID, indexName: indexName})
	admittedMu.Unlock()

	assertIssue5681PublicReadBoundaries(
		t,
		ctx,
		harness,
		executionID,
		indexName,
		environment.deniedCookie,
		environment.secondProjectID,
	)
	expectedClientIdentity := "index-reliability-" + indexName
	sseCancel, sseDone := observeIssue5681SSE(
		t,
		ctx,
		harness,
		executionID,
		expectedClientIdentity,
		expectedClientIdentity,
		[]string{
			controlCanary,
			initialReceipt.SmallImageSHA256,
			initialReceipt.LargeImageSHA256,
			environment.sourceCredentialCanary,
			environment.modelCredentialCanary,
			environment.proxyCredentialCanary,
		},
	)
	harness.waitForJob(t, ctx, executionID, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "DISPATCHED" && snapshot.Published
	}, "published production-scale execution")
	reference := harness.waitForRedisReference(t, ctx, controlCanary, 1)
	assertReferenceOnlyIndexRedis(t, reference)
	assertIssue5681RedisExcludesValues(t, ctx, harness, credentialCanaries)
	commandEvidence := assertIssue5681DecodedRedisReference(
		t,
		ctx,
		harness,
		executionID,
		expectedClientIdentity,
	)
	assertIssue5681StoredBundle(t, ctx, harness, executionID, commandEvidence)
	assertIssue5681BoundedAdmission(
		t,
		ctx,
		harness,
		executionID,
		int64(len(commandEvidence.Entries)),
	)

	entryID := harness.syntheticRead(t, ctx)
	harness.ageSyntheticPending(t, ctx, entryID)
	pending := harness.pendingEntries(t, ctx)
	if len(pending) != 1 || pending[0].ID != entryID ||
		pending[0].Consumer != syntheticConsumer || pending[0].Deliveries < 2 {
		t.Fatalf("crash/retry fixture did not retain one pending reference: %+v", pending)
	}

	harness.startWorker(t, ctx)
	terminal := waitForIssue5681Terminal(t, ctx, harness, executionID)
	if terminal.State != "SUCCEEDED" || terminal.CompletionStatus != "ok" {
		t.Fatalf("production-scale indexing did not succeed: %+v", terminal)
	}
	const expectedCompletion = "Successfully indexed 1 documents (12 chunks)."
	if terminal.CompletionMessage != expectedCompletion {
		t.Fatalf(
			"completion message = %q, want exact current-baseline result %q",
			terminal.CompletionMessage,
			expectedCompletion,
		)
	}
	assertIssue5681DurableTerminal(
		t,
		terminal,
		config.projectID,
		environment.expectedWorkloadIdentity,
	)
	harness.waitForRedisEmpty(t, ctx, controlCanary)
	removeIssue5681SyntheticConsumer(t, ctx, harness)

	sse := finishIssue5681SSE(t, sseCancel, sseDone)
	assertIssue5681SSE(t, sse, terminal, indexName)
	assertIssue5681SSEReconnect(
		t,
		ctx,
		harness,
		executionID,
		expectedClientIdentity,
		expectedClientIdentity,
		[]string{
			controlCanary,
			initialReceipt.SmallImageSHA256,
			initialReceipt.LargeImageSHA256,
			environment.sourceCredentialCanary,
			environment.modelCredentialCanary,
			environment.proxyCredentialCanary,
		},
		sse,
	)

	receipt := waitForIssue5681Receipt(t, ctx, fixtureBaseURL)
	assertIssue5681Receipt(t, receipt)
	assertIssue5681PgVectorOutcome(t, ctx, harness, indexName)

	// A post-terminal worker restart must not create a second claim, result,
	// settlement, replay terminal, source fetch, or model invocation.
	harness.stopWorker(t, ctx)
	harness.startWorker(t, ctx)
	waitForIssue5681StableRestart(t, ctx, harness, executionID, terminal)
	afterRestartReceipt := readIssue5681Receipt(t, ctx, fixtureBaseURL)
	if !equalIssue5681Receipts(receipt, afterRestartReceipt) {
		t.Fatalf(
			"post-terminal restart repeated source/model effects: before=%+v after=%+v",
			receipt,
			afterRestartReceipt,
		)
	}
	harness.waitForRedisEmpty(t, ctx, controlCanary)

	// Stop remains a separate no-authority slice. It must retire the second
	// command before the worker can fetch any of the 62 MiB source corpus.
	cancelIndexName := "rel-5681-stop-" + nonce
	cancelCanary := "issue-5681-stop-canary-" + nonce
	cancelBody := prepareIndexReliabilityRequest(
		t,
		config.startBody,
		cancelIndexName,
		cancelCanary,
	)
	indexNames = append(indexNames, cancelIndexName)
	harness.stopWorker(t, ctx)
	cancelExecution := harness.startIndex(t, ctx, cancelBody)
	appendIssue5681CleanupResource(t, index5681CleanupResource{
		ExecutionID: cancelExecution,
		ProjectID:   config.projectID,
		ToolkitID:   config.toolkitID,
		IndexName:   cancelIndexName,
	})
	admittedMu.Lock()
	admitted = append(admitted, admittedExecution{id: cancelExecution, indexName: cancelIndexName})
	admittedMu.Unlock()
	harness.waitForJob(t, ctx, cancelExecution, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "DISPATCHED" && snapshot.Published
	}, "published production-scale cancellation target")
	assertReferenceOnlyIndexRedis(
		t,
		harness.waitForRedisReference(t, ctx, cancelCanary, 1),
	)
	assertIssue5681RedisExcludesValues(t, ctx, harness, credentialCanaries)
	if err := harness.stopIndex(ctx, cancelExecution, cancelIndexName); err != nil {
		t.Fatalf("cancel production-scale execution: %v", err)
	}
	if err := harness.stopIndex(ctx, cancelExecution, cancelIndexName); err != nil {
		t.Fatalf("repeat production-scale cancellation: %v", err)
	}
	cancelled := harness.waitForJob(t, ctx, cancelExecution, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "CANCELLED" && snapshot.DesiredState == "CANCELLED"
	}, "durably cancelled production-scale execution")
	if cancelled.Claims != 0 || !cancelled.Retired || cancelled.ReplayEvents == 0 {
		t.Fatalf("pre-claim cancellation crossed worker authority: %+v", cancelled)
	}
	harness.waitForRedisEmpty(t, ctx, cancelCanary)
	if current := readIssue5681Receipt(t, ctx, fixtureBaseURL); !equalIssue5681Receipts(receipt, current) {
		t.Fatalf("cancelled execution reached source/model data plane: before=%+v after=%+v", receipt, current)
	}
	harness.startWorker(t, ctx)
	waitForIssue5681CancelledStability(
		t,
		ctx,
		harness,
		cancelExecution,
		cancelled,
		fixtureBaseURL,
		receipt,
		cancelCanary,
	)

	t.Logf(
		"issue #5681 bounded PoV passed (not a production security/recovery proof): source_bytes=%d max_model_request_bytes=%d redis_envelope_bytes=%d replay_events=%d",
		receipt.SourceCompletedBytes,
		receipt.MaxChatRequestBytes,
		reference.MaxFieldBytes,
		terminal.ReplayEvents,
	)
}

// TestExistingComposeIndexIssue5681OwnerCleanup is invoked only by run.sh from
// a fresh, separately bounded process after the main test process exits or is
// killed. The manifest contains resource identities only.
func TestExistingComposeIndexIssue5681OwnerCleanup(t *testing.T) {
	if os.Getenv(index5681OptIn) != "1" || os.Getenv(index5681CleanupOnly) != "1" {
		t.Skip("owner cleanup is run only by the bounded issue #5681 wrapper")
	}
	config := loadIndexReliabilityConfig(t)
	if !strings.HasPrefix(config.composeProject, "elitea-5681-") ||
		config.baseURL != "https://localhost:18443" {
		t.Fatal("owner cleanup requires the exact dedicated issue #5681 environment")
	}
	resources, manifestPath := readIssue5681CleanupResources(t, config)
	if len(resources) == 0 {
		return
	}
	harness := &indexComposeHarness{config: config}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, resource := range resources {
		if err := harness.stopIndex(
			ctx,
			resource.ExecutionID,
			resource.IndexName,
		); err != nil {
			t.Fatalf("owner cleanup Stop %s: %v", resource.ExecutionID, err)
		}
	}
	err := pollIndexReliability(ctx, 250*time.Millisecond, func() (bool, error) {
		for _, resource := range resources {
			snapshot, snapshotErr := issue5681DurableSnapshotResult(
				ctx,
				harness,
				resource.ExecutionID,
			)
			if snapshotErr != nil {
				return false, snapshotErr
			}
			if !isIndexTerminal(snapshot.State) ||
				snapshot.ReleasedClaims != snapshot.Claims ||
				(!snapshot.Retired && !snapshot.CommittedSettlement) {
				return false, nil
			}
		}
		redisState, redisErr := harness.redisReferenceResult(ctx, "")
		if redisErr != nil {
			return false, redisErr
		}
		pending, pendingErr := harness.pendingEntriesResult(ctx)
		if pendingErr != nil {
			return false, pendingErr
		}
		return redisState.Length == 0 &&
			redisState.Mappings == 0 &&
			redisState.EntryCount == 0 &&
			len(pending) == 0, nil
	})
	if err != nil {
		t.Fatalf("owner cleanup did not converge Redis/DB execution state: %v", err)
	}
	for _, resource := range resources {
		if err := cleanupIssue5681Index(ctx, harness, resource.IndexName); err != nil {
			t.Fatalf("owner cleanup delete index %q: %v", resource.IndexName, err)
		}
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal("remove converged owner cleanup manifest")
	}
}

func appendIssue5681CleanupResource(
	t *testing.T,
	resource index5681CleanupResource,
) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(index5681CleanupManifestEnv))
	if !validIssue5681CleanupResource(resource) ||
		!filepath.IsAbs(path) ||
		strings.ContainsAny(path, "\x00\r\n") {
		t.Fatal("invalid owner cleanup manifest resource")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		t.Fatal("owner cleanup manifest parent must be an owner-only directory")
	}
	if existing, statErr := os.Lstat(path); statErr == nil {
		if !existing.Mode().IsRegular() || existing.Mode().Perm()&0o077 != 0 {
			t.Fatal("owner cleanup manifest must be an owner-only regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("inspect owner cleanup manifest")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("open owner cleanup manifest")
	}
	value, err := json.Marshal(resource)
	if err == nil {
		value = append(value, '\n')
		_, err = file.Write(value)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal("durably append owner cleanup resource")
	}
}

func readIssue5681CleanupResources(
	t *testing.T,
	config indexReliabilityConfig,
) ([]index5681CleanupResource, string) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(index5681CleanupManifestEnv))
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		t.Fatal("owner cleanup manifest path must be absolute")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, path
	}
	if err != nil || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 64*1024 {
		t.Fatal("owner cleanup manifest must be one bounded owner-only regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("open owner cleanup manifest")
	}
	defer closeIndexReliabilityResource(t, file, "owner cleanup manifest")
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 8*1024)
	resources := make([]index5681CleanupResource, 0, 2)
	seen := make(map[string]struct{})
	for scanner.Scan() {
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		var resource index5681CleanupResource
		if decoder.Decode(&resource) != nil ||
			decoder.Decode(new(any)) != io.EOF ||
			!validIssue5681CleanupResource(resource) ||
			resource.ProjectID != config.projectID ||
			resource.ToolkitID != config.toolkitID {
			t.Fatal("owner cleanup manifest contains an invalid resource")
		}
		key := resource.ExecutionID + "\x00" + resource.IndexName
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		resources = append(resources, resource)
		if len(resources) > 16 {
			t.Fatal("owner cleanup manifest exceeds its resource bound")
		}
	}
	if err := scanner.Err(); err != nil || len(resources) == 0 {
		t.Fatal("read owner cleanup manifest")
	}
	return resources, path
}

func validIssue5681CleanupResource(resource index5681CleanupResource) bool {
	return executionIDPattern.MatchString(resource.ExecutionID) &&
		resource.ProjectID > 0 &&
		resource.ToolkitID > 0 &&
		len(resource.IndexName) > 0 &&
		len(resource.IndexName) <= 256 &&
		!strings.ContainsAny(resource.IndexName, "\x00\r\n/\\") &&
		url.PathEscape(resource.IndexName) == resource.IndexName
}

func requiredIssue5681Port(t *testing.T) int {
	t.Helper()
	port, err := strconv.Atoi(os.Getenv(index5681FixturePortEnv))
	if err != nil || port < 1024 || port > 65535 {
		t.Fatalf("%s must be an unprivileged TCP port", index5681FixturePortEnv)
	}
	return port
}

func loadIssue5681Environment(
	t *testing.T,
	config indexReliabilityConfig,
) index5681Environment {
	t.Helper()
	if os.Getenv("ELITEA_INDEX_5681_DEDICATED") != "1" ||
		os.Getenv("ELITEA_INDEX_5681_NO_ADVERSARIAL_RECOVERY_CLAIMANT") != "1" ||
		!strings.HasPrefix(config.composeProject, "elitea-5681-") ||
		len(config.composeProject) > 64 {
		t.Fatal("issue #5681 requires an explicitly dedicated elitea-5681-* Compose project")
	}
	deniedCookiePath := requiredAbsoluteFile(t, index5681DeniedCookieFileEnv, true)
	deniedCookie := readIssue5681Cookie(t, deniedCookiePath, index5681DeniedCookieFileEnv)
	if deniedCookie == config.cookie {
		t.Fatal("allowed and permission-denied browser sessions must be distinct")
	}
	environment := index5681Environment{
		deniedCookie:             deniedCookie,
		secondProjectID:          requiredIssue5681PositiveInteger(t, index5681SecondProjectEnv),
		secondToolkitID:          requiredIssue5681PositiveInteger(t, index5681SecondToolkitEnv),
		expectedWorkloadIdentity: requiredIssue5681Text(t, index5681WorkloadIdentityEnv, "spiffe://", 512),
		sourceAuthDigest:         requiredIssue5681SHA256(t, index5681SourceAuthDigestEnv, false),
		modelAuthDigest:          requiredIssue5681SHA256(t, index5681ModelAuthDigestEnv, false),
		proxyAttestationDigest:   requiredIssue5681SHA256(t, index5681ProxyAttestationEnv, false),
		sourceCredentialCanary:   requiredIssue5681Text(t, index5681SourceCanaryEnv, "issue-5681-credential-canary-", 128),
		modelCredentialCanary:    requiredIssue5681Text(t, index5681ModelCanaryEnv, "issue-5681-credential-canary-", 128),
		proxyCredentialCanary:    requiredIssue5681Text(t, index5681ProxyCanaryEnv, "issue-5681-credential-canary-", 128),
		platformSHA:              requiredIssue5681Hex(t, index5681PlatformSHAEnv, 40),
		mainImageID:              requiredIssue5681SHA256(t, index5681MainImageIDEnv, true),
		workerImageID:            requiredIssue5681SHA256(t, index5681WorkerImageIDEnv, true),
		liteLLMImageID:           requiredIssue5681SHA256(t, index5681LiteLLMImageIDEnv, true),
		gatewayImageID:           requiredIssue5681SHA256(t, index5681GatewayImageIDEnv, true),
		gatewayRouteDigest:       requiredIssue5681SHA256(t, index5681GatewayRouteDigestEnv, false),
		gatewayBaseDigest:        requiredIssue5681SHA256(t, index5681GatewayBaseDigestEnv, false),
		liteLLMService:           requiredIssue5681Text(t, index5681LiteLLMServiceEnv, "", 128),
		liteLLMRevision:          requiredIssue5681Hex(t, index5681LiteLLMRevisionEnv, 40),
		sdkRevision:              requiredIssue5681Hex(t, index5681SDKRevisionEnv, 40),
	}
	if environment.secondProjectID == config.projectID {
		t.Fatal("issue #5681 second project must differ from the execution project")
	}
	if environment.sourceCredentialCanary == environment.modelCredentialCanary ||
		environment.sourceCredentialCanary == environment.proxyCredentialCanary ||
		environment.modelCredentialCanary == environment.proxyCredentialCanary {
		t.Fatal("issue #5681 credential canaries must be distinct seeded values")
	}
	if environment.liteLLMService == "" ||
		strings.ContainsAny(environment.liteLLMService, "/\\.:") {
		t.Fatalf("%s must be one bounded Compose service name", index5681LiteLLMServiceEnv)
	}
	return environment
}

func readIssue5681Cookie(t *testing.T, path, environmentName string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s", environmentName)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n") ||
		!strings.Contains(value, "=") || strings.HasPrefix(strings.ToLower(value), "cookie:") {
		t.Fatalf("%s must contain one bounded raw Cookie header value", environmentName)
	}
	return value
}

func requiredIssue5681PositiveInteger(t *testing.T, environmentName string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(environmentName)), 10, 32)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive PostgreSQL integer", environmentName)
	}
	return value
}

func requiredIssue5681Text(
	t *testing.T,
	environmentName string,
	prefix string,
	maximum int,
) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(environmentName))
	if !strings.HasPrefix(value, prefix) || len(value) > maximum ||
		strings.ContainsAny(value, "\x00\r\n") {
		t.Fatalf("%s has an invalid non-secret identity", environmentName)
	}
	return value
}

func requiredIssue5681SHA256(
	t *testing.T,
	environmentName string,
	withPrefix bool,
) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(environmentName))
	digest := value
	if withPrefix {
		if !strings.HasPrefix(value, "sha256:") {
			t.Fatalf("%s must be an immutable sha256 image ID", environmentName)
		}
		digest = strings.TrimPrefix(value, "sha256:")
	}
	if len(digest) != sha256.Size*2 {
		t.Fatalf("%s must contain one SHA-256 digest", environmentName)
	}
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		t.Fatalf("%s must contain lowercase hexadecimal SHA-256", environmentName)
	}
	return value
}

func requiredIssue5681Hex(t *testing.T, environmentName string, length int) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(environmentName))
	if len(value) != length {
		t.Fatalf("%s must contain one full immutable revision", environmentName)
	}
	if _, err := hex.DecodeString(value); err != nil || strings.ToLower(value) != value {
		t.Fatalf("%s must contain lowercase hexadecimal", environmentName)
	}
	return value
}

func requireIssue5681SUTProvenance(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	environment index5681Environment,
) {
	t.Helper()
	repositoryRoot := findRepositoryRoot(t)
	if harness.config.baseURL != "https://localhost:18443" {
		t.Fatal("issue #5681 gate is bound to the dedicated local HTTPS gateway origin")
	}
	head := issue5681CommandOutput(t, ctx, repositoryRoot, "git", "rev-parse", "HEAD")
	if strings.TrimSpace(head) != environment.platformSHA {
		t.Fatalf("checked-out platform revision does not match %s", index5681PlatformSHAEnv)
	}
	if status := issue5681CommandOutput(
		t,
		ctx,
		repositoryRoot,
		"git", "status", "--porcelain", "--untracked-files=all",
	); strings.TrimSpace(status) != "" {
		t.Fatal("production-scale gate requires a completely clean platform checkout")
	}

	for service, expected := range map[string]struct {
		imageID  string
		revision string
	}{
		"elitea-main": {
			imageID:  environment.mainImageID,
			revision: environment.platformSHA,
		},
		indexWorkerService: {
			imageID:  environment.workerImageID,
			revision: environment.platformSHA,
		},
		environment.liteLLMService: {
			imageID:  environment.liteLLMImageID,
			revision: environment.liteLLMRevision,
		},
		"auth_gateway": {
			imageID: environment.gatewayImageID,
		},
	} {
		containerID, err := harness.compose(ctx, "ps", "-q", service)
		if err != nil || strings.TrimSpace(containerID) == "" ||
			strings.ContainsAny(strings.TrimSpace(containerID), "\r\n") {
			t.Fatalf("attest running %s container identity", service)
		}
		if expected.revision == "" {
			imageID := issue5681CommandOutput(
				t,
				ctx,
				repositoryRoot,
				"docker", "inspect", "--format={{.Image}}", strings.TrimSpace(containerID),
			)
			if strings.TrimSpace(imageID) != expected.imageID {
				t.Fatalf("running %s image ID attestation mismatch", service)
			}
		} else {
			attestation := issue5681CommandOutput(
				t,
				ctx,
				repositoryRoot,
				"docker", "inspect",
				`--format={{.Image}}|{{index .Config.Labels "org.opencontainers.image.revision"}}`,
				strings.TrimSpace(containerID),
			)
			if strings.TrimSpace(attestation) != expected.imageID+"|"+expected.revision {
				t.Fatalf("running %s image ID/source revision attestation mismatch", service)
			}
		}
	}
	requireIssue5681GatewayTLSBinding(t, ctx, harness, environment)

	var lock struct {
		DistributionVersion string `json:"distribution_version"`
		Source              struct {
			Revision string `json:"revision"`
		} `json:"source"`
		InstalledPackageTree struct {
			SHA256 string `json:"sha256"`
		} `json:"installed_package_tree"`
		IndexingCapabilityProfile struct {
			ProfileSHA256 string `json:"profile_sha256"`
		} `json:"indexing_capability_profile"`
	}
	lockBytes, err := os.ReadFile(filepath.Join(
		repositoryRoot,
		"services", "elitea-worker-python", "elitea-sdk.lock.json",
	))
	if err != nil || json.Unmarshal(lockBytes, &lock) != nil {
		t.Fatal("read checked-out worker SDK lock")
	}
	if lock.Source.Revision != environment.sdkRevision ||
		lock.DistributionVersion == "" ||
		len(lock.InstalledPackageTree.SHA256) != 64 ||
		len(lock.IndexingCapabilityProfile.ProfileSHA256) != 64 {
		t.Fatal("checked-out worker SDK lock does not match the expected immutable SDK revision")
	}

	const workerAttestation = `import importlib.metadata,json
from elitea_worker.constants import SDK_PACKAGE_TREE_SHA256,SDK_SOURCE_REVISION
from elitea_worker.indexing_runtime_capabilities import require_indexing_runtime_capabilities
print(json.dumps({"revision":SDK_SOURCE_REVISION,"tree":SDK_PACKAGE_TREE_SHA256,"version":importlib.metadata.version("elitea-sdk"),"profile":require_indexing_runtime_capabilities()},sort_keys=True))`
	output, err := harness.compose(
		ctx,
		"exec", "-T", indexWorkerService,
		"python", "-c", workerAttestation,
	)
	if err != nil {
		t.Fatal("attest the running worker SDK and indexing capability profile")
	}
	var actual struct {
		Revision string `json:"revision"`
		Tree     string `json:"tree"`
		Version  string `json:"version"`
		Profile  string `json:"profile"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &actual) != nil ||
		actual.Revision != lock.Source.Revision ||
		actual.Tree != lock.InstalledPackageTree.SHA256 ||
		actual.Version != lock.DistributionVersion ||
		actual.Profile != lock.IndexingCapabilityProfile.ProfileSHA256 {
		t.Fatal("running worker SDK/capability attestation does not match the checked-out lock")
	}
}

func issue5681CommandOutput(
	t *testing.T,
	ctx context.Context,
	directory string,
	name string,
	arguments ...string,
) string {
	t.Helper()
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		t.Fatalf("execute non-secret provenance command %s", name)
	}
	return stdout.String()
}

func requireIssue5681GatewayTLSBinding(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	environment index5681Environment,
) {
	t.Helper()
	containerID, err := harness.compose(ctx, "ps", "-q", "auth_gateway")
	if err != nil || strings.TrimSpace(containerID) == "" {
		t.Fatal("resolve dedicated auth gateway container")
	}
	mountOutput := issue5681CommandOutput(
		t,
		ctx,
		findRepositoryRoot(t),
		"docker", "inspect", "--format={{json .Mounts}}", strings.TrimSpace(containerID),
	)
	var mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(mountOutput)), &mounts) != nil {
		t.Fatal("decode auth gateway mount attestation")
	}
	expectedMounts := map[string]string{
		"/etc/traefik/dynamic/base.yml": filepath.Join(
			harness.config.centryDir,
			"hybrid_auth",
			"traefik-dynamic.yml",
		),
		"/etc/traefik/dynamic/index.yml": filepath.Join(
			harness.config.centryDir,
			"hybrid_auth",
			"traefik-index-routes.yml",
		),
		"/run/elitea-auth/gateway.crt": filepath.Join(
			harness.config.runtimeDir,
			"runtime",
			"gateway-server.crt",
		),
	}
	for destination, expectedSource := range expectedMounts {
		found := false
		for _, mount := range mounts {
			if mount.Destination == destination &&
				mount.Type == "bind" &&
				!mount.RW &&
				filepath.Clean(mount.Source) == filepath.Clean(expectedSource) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("auth gateway lacks exact read-only %s mount", destination)
		}
	}

	routeBytes := readIssue5681AttestedFile(
		t,
		expectedMounts["/etc/traefik/dynamic/index.yml"],
		environment.gatewayRouteDigest,
	)
	baseBytes := readIssue5681AttestedFile(
		t,
		expectedMounts["/etc/traefik/dynamic/base.yml"],
		environment.gatewayBaseDigest,
	)
	route := string(routeBytes)
	routerStart := strings.Index(route, "    go-current-index-runtime:\n")
	routerEnd := strings.Index(route, "\n    runtime-worker-current-tools-list:")
	if routerStart < 0 || routerEnd <= routerStart {
		t.Fatal("attested gateway route lacks the exact index-runtime router")
	}
	router := route[routerStart:routerEnd]
	for _, required := range []string{
		"PathRegexp(`^/api/v2/elitea_core/test_toolkit_tool/prompt_lib/[1-9][0-9]*$`)",
		"Query(`execution_contract`, `index.ingest.v1`)",
		"PathRegexp(`^/api/v2/elitea_core/regenerate/prompt_lib/[1-9][0-9]*/[^/]+$`)",
		"Query(`execution_contract`, `agent.regenerate.v1`)",
		"PathRegexp(`^/api/v2/elitea_core/task/prompt_lib/[1-9][0-9]*/[^/]+$`)",
		"PathRegexp(`^/api/v2/elitea_core/index_cancel/prompt_lib/",
		"PathRegexp(`^/api/v2/elitea_core/index_meta/prompt_lib/",
		"PathRegexp(`^/api/v2/executions/[1-9][0-9]*/[^/]+/events$`)",
		"priority: 90",
		"entryPoints: [websecure]",
		"tls: {}",
		"middlewares:\n        - strip-caller-auth-context\n        - normalize-runtime-public-authority\n        - go-main-forward-auth",
		"service: elitea-main",
	} {
		if strings.Count(router, required) != 1 {
			t.Fatal("attested gateway index router has an unexpected semantic contract")
		}
	}
	if strings.Contains(router, "PathPrefix(") ||
		strings.Contains(router, "service: current-main") {
		t.Fatal("attested gateway index router is broader than the Go runtime route")
	}
	requireNestedApplicationReferenceRouter(t, route)
	base := string(baseBytes)
	for _, required := range []string{
		"normalize-runtime-public-authority:",
		"Host: localhost:18443",
		"strip-caller-auth-context:",
		`X-Auth-Type: ""`,
		`X-Auth-ID: ""`,
		`X-Auth-Reference: ""`,
		"go-main-forward-auth:",
		"address: http://elitea-main-auth:8080/internal/forward-auth/main",
		"trustForwardHeader: false",
		"forwardBody: false",
		"preserveRequestMethod: false",
	} {
		if strings.Count(base, required) != 1 {
			t.Fatal("attested gateway middleware contract is incomplete or ambiguous")
		}
	}
	serviceStart := strings.Index(base, "    elitea-main:\n")
	serviceEnd := strings.Index(base, "\n    current-main:")
	if serviceStart < 0 || serviceEnd <= serviceStart ||
		base[serviceStart:serviceEnd] !=
			"    elitea-main:\n      loadBalancer:\n        servers:\n          - url: http://elitea-main-auth:8080\n" {
		t.Fatal("attested gateway elitea-main service must have exactly one auth-bound target")
	}

	certificateBytes, err := os.ReadFile(expectedMounts["/run/elitea-auth/gateway.crt"])
	if err != nil {
		t.Fatal("read dedicated gateway certificate")
	}
	block, rest := pem.Decode(certificateBytes)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("dedicated gateway certificate must contain exactly one PEM certificate")
	}
	rootsBytes, err := os.ReadFile(filepath.Join(
		harness.config.runtimeDir,
		"runtime",
		"runtime-ca.crt",
	))
	if err != nil {
		t.Fatal("read dedicated runtime CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootsBytes) {
		t.Fatal("dedicated runtime CA contains no certificate")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	connection, err := tls.DialWithDialer(dialer, "tcp", "localhost:18443", &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatal("establish dedicated gateway TLS connection")
	}
	defer closeIndexReliabilityResource(t, connection, "dedicated gateway TLS connection")
	state := connection.ConnectionState()
	if len(state.PeerCertificates) == 0 ||
		!bytes.Equal(state.PeerCertificates[0].Raw, block.Bytes) {
		t.Fatal("public HTTPS origin is not serving the attested gateway certificate")
	}
}

func readIssue5681AttestedFile(
	t *testing.T,
	path string,
	expectedDigest string,
) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil || len(value) == 0 || len(value) > 64*1024 {
		t.Fatal("read bounded gateway configuration")
	}
	digest := sha256.Sum256(value)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		t.Fatal("gateway configuration digest does not match the operator attestation")
	}
	return value
}

func requireIssue5681CleanDedicatedBaseline(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
) {
	t.Helper()
	if count := harness.postgresScalar(
		t,
		ctx,
		`SELECT count(*) FROM elitea_runtime.execution_jobs`,
	); count != "0" {
		t.Fatalf("dedicated Compose project contains pre-existing runtime executions: count=%s", count)
	}
	stream, err := harness.redisReferenceResult(ctx, "")
	if err != nil {
		t.Fatalf("inspect dedicated issue #5681 Redis baseline: %v", err)
	}
	if stream.Length != 0 || stream.Mappings != 0 || stream.EntryCount != 0 {
		t.Fatalf("dedicated issue #5681 Redis baseline is not empty: %+v", stream)
	}
	pending, err := harness.pendingEntriesResult(ctx)
	if err != nil {
		t.Fatalf("inspect dedicated issue #5681 Redis pending baseline: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("dedicated issue #5681 Redis consumer group is not empty: %+v", pending)
	}
}

func requireIssue5681RedisNoEviction(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
) {
	t.Helper()
	output, err := harness.compose(
		ctx,
		"exec", "-T", "runtime_redis",
		"sh", "-ec", `tr '\000' '\n' </proc/1/cmdline`,
	)
	if err != nil {
		t.Fatal("attest dedicated runtime Redis process configuration")
	}
	arguments := strings.Fields(output)
	hasPair := func(flag, value string) bool {
		for index := 0; index+1 < len(arguments); index++ {
			if arguments[index] == flag && arguments[index+1] == value {
				return true
			}
		}
		return false
	}
	if !hasPair("--maxmemory", "64mb") ||
		!hasPair("--maxmemory-policy", "noeviction") ||
		!hasPair("--appendonly", "yes") ||
		!hasPair("--appendfsync", "everysec") {
		t.Fatal("runtime Redis lacks the exact bounded noeviction/durable process profile")
	}
}

func assertIssue5681RedisExcludesValues(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	forbiddenValues []string,
) {
	t.Helper()
	const script = `
local rows = redis.call('XRANGE', KEYS[1], '-', '+')
local mappings = redis.call('HGETALL', KEYS[2])
for _, needle in ipairs(ARGV) do
  if needle == '' then return 1 end
  for _, row in ipairs(rows) do
    for _, value in ipairs(row[2]) do
      if string.find(value, needle, 1, true) then return 1 end
    end
  end
  for _, value in ipairs(mappings) do
    if string.find(value, needle, 1, true) then return 1 end
  end
end
return 0`
	arguments := []string{
		"--raw", "EVAL", script, "2",
		indexCommandStream, indexCommandStream + ":delivery-index.v1",
	}
	arguments = append(arguments, forbiddenValues...)
	output, err := harness.redis(ctx, "producer", arguments...)
	if err != nil || strings.TrimSpace(output) != "0" {
		t.Fatal("Redis control state contains a seeded credential canary")
	}
}

func requireIssue5681RequestProfile(t *testing.T, body map[string]any) {
	t.Helper()
	params, ok := body["tool_params"].(map[string]any)
	if !ok {
		t.Fatal("issue #5681 request tool_params must be an object")
	}
	if params["include_attachments"] != true || params["bins_with_llm"] != true {
		t.Fatal("issue #5681 request must enable include_attachments and bins_with_llm")
	}
	if value, ok := params["max_pages"].(json.Number); !ok || value != "1" {
		t.Fatal("issue #5681 request max_pages must be exactly 1")
	}
}

func assertIssue5681AdmissionRBAC(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	body []byte,
	deniedCookie string,
) {
	t.Helper()
	endpoint := fmt.Sprintf(
		"%s/api/v2/elitea_core/test_toolkit_tool/prompt_lib/%d?await_response=false&execution_contract=index.ingest.v1",
		harness.config.baseURL,
		harness.config.projectID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "spoofed-forward-auth")
	request.Header.Set("X-Auth-Id", "1")
	request.Header.Set("X-Auth-Reference", "attacker@example.invalid")
	response, err := harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("exercise unauthenticated index admission: %v", err)
	}
	defer closeIndexReliabilityResource(
		t,
		response.Body,
		"unauthenticated index admission response",
	)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated forwarded identity reached index admission: status=%d", response.StatusCode)
	}

	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", deniedCookie)
	response, err = harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("exercise permission-denied index admission: %v", err)
	}
	defer closeIndexReliabilityResource(
		t,
		response.Body,
		"permission-denied index admission response",
	)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("authenticated actor without index permission reached admission: status=%d", response.StatusCode)
	}
}

func assertIssue5681PublicReadBoundaries(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	indexName string,
	deniedCookie string,
	secondProjectID int64,
) {
	t.Helper()
	endpoint := fmt.Sprintf(
		"%s/api/v2/executions/%d/%s/events",
		harness.config.baseURL,
		harness.config.projectID,
		executionID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Auth-Type", "spoofed-forward-auth")
	request.Header.Set("X-Auth-Id", "1")
	request.Header.Set("X-Auth-Reference", "attacker@example.invalid")
	response, err := harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("exercise unauthenticated SSE: %v", err)
	}
	defer closeIndexReliabilityResource(t, response.Body, "unauthenticated SSE response")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated forwarded identity reached SSE: status=%d", response.StatusCode)
	}

	request, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", deniedCookie)
	response, err = harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("exercise permission-denied SSE: %v", err)
	}
	defer closeIndexReliabilityResource(t, response.Body, "permission-denied SSE response")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("authenticated actor without replay permission reached SSE: status=%d", response.StatusCode)
	}

	stopEndpoint := fmt.Sprintf(
		"%s/api/v2/elitea_core/index_cancel/prompt_lib/%d/%d/%s/%s",
		harness.config.baseURL,
		harness.config.projectID,
		harness.config.toolkitID,
		url.PathEscape(indexName),
		executionID,
	)
	request, err = http.NewRequestWithContext(ctx, http.MethodDelete, stopEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", deniedCookie)
	response, err = harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("exercise permission-denied Stop: %v", err)
	}
	defer closeIndexReliabilityResource(t, response.Body, "permission-denied Stop response")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("authenticated actor without task-delete permission reached Stop: status=%d", response.StatusCode)
	}

	secondProjectEndpoint := fmt.Sprintf(
		"%s/api/v2/executions/%d/%s/events",
		harness.config.baseURL,
		secondProjectID,
		executionID,
	)
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, secondProjectEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", harness.config.cookie)
	response, err = harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("exercise cross-tenant SSE: %v", err)
	}
	defer closeIndexReliabilityResource(t, response.Body, "cross-tenant SSE response")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("execution replay crossed its project boundary: status=%d", response.StatusCode)
	}
}

func requireIssue5681SecondProjectAccess(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	projectID int64,
	toolkitID int64,
) {
	t.Helper()
	endpoint := fmt.Sprintf(
		"%s/api/v2/elitea_core/test_toolkit_tool/prompt_lib/%d?await_response=false&execution_contract=index.ingest.v1",
		harness.config.baseURL,
		projectID,
	)
	body := []byte(fmt.Sprintf(`{"toolkit_config":{"toolkit_id":%d}}`, toolkitID))
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", harness.config.cookie)
	response, err := harness.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("verify allowed actor replay permission in the second project: %v", err)
	}
	defer closeIndexReliabilityResource(t, response.Body, "second-project admission response")
	// The deliberately incomplete body reaches the start handler only after
	// the exact models.applications.tool.patch project permission succeeds.
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("allowed actor lacks replay-equivalent permission in the second project: status=%d", response.StatusCode)
	}
}

func requireIssue5681ConfluenceToolkit(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
) {
	t.Helper()
	query := fmt.Sprintf(
		`SELECT type FROM p_%d.elitea_tools WHERE id = %d`,
		harness.config.projectID,
		harness.config.toolkitID,
	)
	toolkitType := harness.postgresScalar(t, ctx, query)
	if toolkitType != "confluence" {
		t.Fatalf("issue #5681 toolkit type = %q, want confluence", toolkitType)
	}
}

func requireWorkerCanReachIssue5681Fixture(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	port int,
) {
	t.Helper()
	url := fmt.Sprintf("http://host.docker.internal:%d/__elitea_issue_5681/health", port)
	const probe = `import json,sys,urllib.request
with urllib.request.urlopen(sys.argv[1], timeout=5) as response:
    value=json.load(response)
if value.get("status") != "ready":
    raise SystemExit(2)`
	if _, err := harness.compose(
		ctx,
		"exec", "-T", indexWorkerService,
		"python", "-c", probe, url,
	); err != nil {
		t.Fatalf(
			"worker cannot reach fixture; provision Confluence URL as http://host.docker.internal:%d: %v",
			port,
			err,
		)
	}
}

func startIssue5681Fixture(
	t *testing.T,
	ctx context.Context,
	projectID int64,
	port int,
	environment index5681Environment,
) *index5681FixtureProcess {
	t.Helper()
	repositoryRoot := findRepositoryRoot(t)
	script := filepath.Join(
		repositoryRoot,
		"services", "elitea-main", "tests", "reliability", "index_5681",
		"fixture_server.py",
	)
	python := os.Getenv(index5681PythonEnv)
	if python == "" {
		python = "python3"
	}
	resolvedPython, err := exec.LookPath(python)
	if err != nil {
		t.Fatalf("resolve issue #5681 fixture Python: %v", err)
	}
	fixtureRoot := filepath.Join(canonicalTempDir(t), "issue-5681-fixture")
	command := exec.CommandContext(
		ctx,
		resolvedPython,
		script,
		"--port", strconv.Itoa(port),
		"--project-id", strconv.FormatInt(projectID, 10),
		"--root", fixtureRoot,
		"--source-authorization-sha256", environment.sourceAuthDigest,
		"--model-authorization-sha256", environment.modelAuthDigest,
		"--proxy-attestation-sha256", environment.proxyAttestationDigest,
		"--source-credential-canary", environment.sourceCredentialCanary,
		"--model-credential-canary", environment.modelCredentialCanary,
		"--proxy-credential-canary", environment.proxyCredentialCanary,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open fixture stdout: %v", err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatalf("start issue #5681 fixture: %v", err)
	}
	process := &index5681FixtureProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()

	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				ready <- err
			} else {
				ready <- errors.New("fixture exited before readiness")
			}
			return
		}
		var value struct {
			Status             string `json:"status"`
			Schema             string `json:"schema"`
			SourcePayloadBytes int64  `json:"source_payload_bytes"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			ready <- fmt.Errorf("decode fixture readiness: %w", err)
			return
		}
		if value.Status != "ready" || value.Schema != index5681FixtureProfile ||
			value.SourcePayloadBytes != index5681FixtureBytes {
			ready <- fmt.Errorf("unexpected fixture readiness profile: %+v", value)
			return
		}
		ready <- nil
	}()

	select {
	case err := <-ready:
		if err != nil {
			process.stop(t)
			t.Fatalf("issue #5681 fixture readiness: %v", err)
		}
	case <-process.done:
		t.Fatalf("issue #5681 fixture exited early: %v", process.waitError())
	case <-time.After(time.Minute):
		process.stop(t)
		t.Fatal("issue #5681 fixture did not generate the deterministic corpus within one minute")
	}
	return process
}

func (p *index5681FixtureProcess) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		if p.command.Process == nil {
			return
		}
		_ = p.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = p.command.Process.Kill()
			<-p.done
		}
	})
}

func (p *index5681FixtureProcess) waitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func waitForIssue5681Fixture(
	t *testing.T,
	ctx context.Context,
	baseURL string,
	process *index5681FixtureProcess,
) {
	t.Helper()
	err := pollIndexReliability(ctx, 100*time.Millisecond, func() (bool, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/__elitea_issue_5681/health", nil)
		if err != nil {
			return false, err
		}
		response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if err != nil {
			select {
			case <-process.done:
				return false, fmt.Errorf("fixture exited: %w", process.waitError())
			default:
			}
			return false, err
		}
		matched := response.StatusCode == http.StatusOK
		if err := response.Body.Close(); err != nil {
			return false, fmt.Errorf("close issue #5681 fixture health response: %w", err)
		}
		return matched, nil
	})
	if err != nil {
		t.Fatalf("wait for issue #5681 fixture: %v", err)
	}
}

func assertIssue5681DecodedRedisReference(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	expectedClientIdentity string,
) index5681DecodedCommandEvidence {
	t.Helper()
	const script = `
local rows = redis.call('XRANGE', KEYS[1], '-', '+', 'COUNT', 2)
local mappings = redis.call('HGETALL', KEYS[2])
if #rows ~= 1 or #rows[1][2] ~= 2 or #mappings ~= 2 then return '{}' end
local function hex(value)
  return (string.gsub(value, '.', function(character)
    return string.format('%02x', string.byte(character))
  end))
end
return cjson.encode({
  entry_id = rows[1][1],
  field_name = rows[1][2][1],
  envelope_hex = hex(rows[1][2][2]),
  mapping_field = mappings[1],
  mapping_value = mappings[2],
  stream_length = redis.call('XLEN', KEYS[1]),
  mapping_count = redis.call('HLEN', KEYS[2]),
  field_count = 1,
  envelope_bytes = string.len(rows[1][2][2]),
  mapping_field_bytes = string.len(mappings[1]),
  mapping_value_bytes = string.len(mappings[2])
})`
	output, err := harness.redis(
		ctx,
		"producer",
		"--raw", "EVAL", script, "2",
		indexCommandStream, indexCommandStream+":delivery-index.v1",
	)
	if err != nil {
		t.Fatalf("read issue #5681 Redis command evidence: %v", err)
	}
	var evidence index5681RedisEvidence
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &evidence) != nil ||
		evidence.StreamLength != 1 ||
		evidence.MappingCount != 1 ||
		evidence.FieldCount != 1 ||
		evidence.FieldName != "signed_envelope" ||
		!redisEntryPattern.MatchString(evidence.EntryID) ||
		evidence.MappingValue != evidence.EntryID ||
		evidence.EnvelopeBytes <= 0 ||
		evidence.EnvelopeBytes > 48*1024 ||
		evidence.MappingFieldBytes <= 0 ||
		evidence.MappingFieldBytes > 256 ||
		evidence.MappingValueBytes <= 0 ||
		evidence.MappingValueBytes > 64 {
		t.Fatal("invalid Redis command/delivery-index evidence")
	}
	envelopeBytes, err := hex.DecodeString(evidence.EnvelopeHex)
	if err != nil || int64(len(envelopeBytes)) != evidence.EnvelopeBytes {
		t.Fatal("Redis signed envelope hex evidence is malformed")
	}
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	if err := runtimegrpc.ScanStrictMessage(
		envelopeBytes,
		envelope.ProtoReflect().Descriptor(),
	); err != nil {
		t.Fatalf("strictly scan Redis signed envelope: %v", err)
	}
	if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
		t.Fatalf("decode Redis signed envelope: %v", err)
	}
	canonicalEnvelope, err := proto.MarshalOptions{Deterministic: true}.Marshal(&envelope)
	if err != nil || !bytes.Equal(canonicalEnvelope, envelopeBytes) ||
		envelope.GetEnvelopeSchemaRevision() != "elitea.runtime.signed-worker-command.v1" ||
		envelope.GetSignatureProfile() != runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_ED25519 ||
		envelope.GetKeyId() == "" ||
		len(envelope.GetSignature()) != 64 ||
		!validIssue5681Digest(envelope.GetWorkerCommandDigest(), envelope.GetWorkerCommandBytes()) {
		t.Fatal("Redis signed envelope is not the exact canonical production contract")
	}

	commandBytes := envelope.GetWorkerCommandBytes()
	var command runtimev1.WorkerCommandV1
	if err := runtimegrpc.ScanStrictMessage(
		commandBytes,
		command.ProtoReflect().Descriptor(),
	); err != nil {
		t.Fatalf("strictly scan Redis worker command: %v", err)
	}
	if err := proto.Unmarshal(commandBytes, &command); err != nil {
		t.Fatalf("decode Redis worker command: %v", err)
	}
	canonicalCommand, err := proto.MarshalOptions{Deterministic: true}.Marshal(&command)
	indexCommand := command.GetIndexIngest()
	projectID := strconv.FormatInt(harness.config.projectID, 10)
	bundle := command.GetInputBundleRef()
	if err != nil || !bytes.Equal(canonicalCommand, commandBytes) ||
		command.GetProtocolRevision() != "elitea.runtime.v1" ||
		command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST ||
		command.GetExecutionId() != executionID ||
		command.GetGeneration() != 1 ||
		command.GetRootExecutionId() != executionID ||
		command.GetTenantId() != projectID ||
		command.GetResourceProjectId() != projectID ||
		command.GetProjectionProjectId() != projectID ||
		command.GetCapabilityId() != "index.ingest.v1" ||
		command.GetCapabilityVersion() != "1" ||
		command.GetCommandId() == "" ||
		command.GetIdempotencyKey() != evidence.MappingField ||
		indexCommand == nil ||
		indexCommand.GetClientStreamId() != expectedClientIdentity ||
		indexCommand.GetClientMessageId() != expectedClientIdentity ||
		indexCommand.GetSioEvent() != "chat_predict" ||
		bundle == nil ||
		bundle.GetInputBundleId() == "" ||
		bundle.GetImmutableVersion() == "" ||
		bundle.GetByteLength() == 0 ||
		bundle.GetByteLength() > 64*1024 ||
		bundle.GetMediaType() == "" ||
		!validIssue5681DigestValue(bundle.GetDigest()) {
		t.Fatal("Redis worker command is not the exact bounded index.ingest.v1 reference contract")
	}

	references := []index5681CommandEntryReference{
		{
			EntryID:      indexCommand.GetToolkitConfigurationEntryId(),
			SemanticRole: "index.toolkit_configuration",
		},
		{
			EntryID:      indexCommand.GetToolParametersEntryId(),
			SemanticRole: "index.tool_parameters",
		},
		{
			EntryID:      indexCommand.GetLlmModelEntryId(),
			SemanticRole: "index.llm_model",
		},
		{
			EntryID:      indexCommand.GetLlmConfigurationEntryId(),
			SemanticRole: "index.llm_configuration",
		},
		{
			EntryID:      indexCommand.GetMcpTokensEntryId(),
			SemanticRole: "index.mcp_tokens",
		},
	}
	if references[0].EntryID == "" || references[1].EntryID == "" {
		t.Fatal("Redis index command lacks required toolkit/tool-parameter references")
	}
	if binding := indexCommand.GetEmbeddingBinding(); binding != nil {
		if binding.GetEntryId() == "" ||
			binding.GetImmutableVersion() == "" ||
			!validIssue5681DigestValue(binding.GetContentDigest()) {
			t.Fatal("Redis index command embedding binding is malformed")
		}
		references = append(references, index5681CommandEntryReference{
			EntryID:          binding.GetEntryId(),
			SemanticRole:     "index.embedding_binding",
			ImmutableVersion: binding.GetImmutableVersion(),
			DigestHex:        hex.EncodeToString(binding.GetContentDigest().GetValue()),
		})
	}
	seen := make(map[string]struct{}, len(references))
	nonEmpty := references[:0]
	for _, reference := range references {
		if reference.EntryID == "" {
			continue
		}
		if len(reference.EntryID) > 256 {
			t.Fatal("Redis index command contains an oversized input-entry identity")
		}
		if _, duplicate := seen[reference.EntryID]; duplicate {
			t.Fatal("Redis index command aliases two immutable input entries")
		}
		seen[reference.EntryID] = struct{}{}
		nonEmpty = append(nonEmpty, reference)
	}
	if len(nonEmpty) < 2 || len(nonEmpty) > 6 {
		t.Fatalf("Redis index command references %d input entries, want 2..6", len(nonEmpty))
	}
	return index5681DecodedCommandEvidence{
		BundleID:        bundle.GetInputBundleId(),
		BundleVersion:   bundle.GetImmutableVersion(),
		BundleDigestHex: hex.EncodeToString(bundle.GetDigest().GetValue()),
		BundleBytes:     int64(bundle.GetByteLength()),
		BundleMediaType: bundle.GetMediaType(),
		Entries:         nonEmpty,
	}
}

func assertIssue5681StoredBundle(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	command index5681DecodedCommandEvidence,
) {
	t.Helper()
	if !executionIDPattern.MatchString(executionID) {
		t.Fatal("invalid execution identity for bundle evidence")
	}
	query := fmt.Sprintf(`
SELECT json_build_object(
  'bundle_id', b.input_bundle_id,
  'bundle_version', b.immutable_version,
  'media_type', b.media_type,
  'digest_hex', encode(b.manifest_digest, 'hex'),
  'manifest_size', b.manifest_size,
  'manifest_hex', encode(b.manifest_bytes, 'hex'),
  'entries', COALESCE((
    SELECT json_agg(json_build_object(
      'entry_id', e.entry_id,
      'entry_version', e.entry_version,
      'semantic_role', e.semantic_role,
      'media_type', e.media_type,
      'digest_hex', encode(e.content_digest, 'hex'),
      'content_size', e.content_size,
      'content_reference', e.content_reference,
      'classification', e.classification,
      'required_grant_audience', e.required_grant_audience
    ) ORDER BY e.semantic_role)
    FROM elitea_runtime.input_bundle_entries AS e
    WHERE e.input_bundle_id = b.input_bundle_id
  ), '[]'::json)
)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.input_bundles AS b ON b.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = '%s'
  AND j.capability_id = 'index.ingest.v1'`, executionID)
	output, err := harness.postgres(ctx, query)
	if err != nil || len(output) > 256*1024 {
		t.Fatal("read bounded immutable input-bundle evidence")
	}
	var stored struct {
		BundleID      string `json:"bundle_id"`
		BundleVersion string `json:"bundle_version"`
		MediaType     string `json:"media_type"`
		DigestHex     string `json:"digest_hex"`
		ManifestSize  int64  `json:"manifest_size"`
		ManifestHex   string `json:"manifest_hex"`
		Entries       []struct {
			EntryID               string `json:"entry_id"`
			EntryVersion          string `json:"entry_version"`
			SemanticRole          string `json:"semantic_role"`
			MediaType             string `json:"media_type"`
			DigestHex             string `json:"digest_hex"`
			ContentSize           int64  `json:"content_size"`
			ContentReference      string `json:"content_reference"`
			Classification        string `json:"classification"`
			RequiredGrantAudience string `json:"required_grant_audience"`
		} `json:"entries"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &stored) != nil ||
		stored.BundleID != command.BundleID ||
		stored.BundleVersion != command.BundleVersion ||
		stored.MediaType != command.BundleMediaType ||
		stored.DigestHex != command.BundleDigestHex ||
		stored.ManifestSize != command.BundleBytes ||
		len(stored.Entries) != len(command.Entries) ||
		stored.ManifestSize <= 0 ||
		stored.ManifestSize > 64*1024 {
		t.Fatal("stored input bundle does not match the decoded Redis reference")
	}
	manifestBytes, err := hex.DecodeString(stored.ManifestHex)
	if err != nil || int64(len(manifestBytes)) != stored.ManifestSize {
		t.Fatal("stored input-bundle manifest bytes are malformed")
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(manifestDigest[:]) != stored.DigestHex {
		t.Fatal("stored input-bundle manifest digest does not match exact bytes")
	}
	var manifest runtimev1.ExecutionInputBundleV1
	if err := runtimegrpc.ScanStrictMessage(
		manifestBytes,
		manifest.ProtoReflect().Descriptor(),
	); err != nil {
		t.Fatalf("strictly scan stored input-bundle manifest: %v", err)
	}
	if err := proto.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode stored input-bundle manifest: %v", err)
	}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(&manifest)
	if err != nil || !bytes.Equal(canonical, manifestBytes) ||
		manifest.GetInputBundleId() != stored.BundleID ||
		manifest.GetImmutableVersion() != stored.BundleVersion ||
		len(manifest.GetEntries()) != len(stored.Entries) {
		t.Fatal("stored input-bundle manifest is not the exact canonical contract")
	}

	type entryEvidence struct {
		version        string
		role           string
		mediaType      string
		digestHex      string
		contentSize    int64
		contentRef     string
		classification string
		audience       string
	}
	storedByID := make(map[string]entryEvidence, len(stored.Entries))
	for _, entry := range stored.Entries {
		if entry.EntryID == "" || entry.EntryVersion == "" ||
			entry.SemanticRole == "" || entry.MediaType != "application/json" ||
			len(entry.DigestHex) != sha256.Size*2 ||
			entry.ContentSize <= 0 || entry.ContentSize > 256*1024 ||
			entry.ContentReference == "" || entry.Classification == "" ||
			entry.RequiredGrantAudience == "" {
			t.Fatal("stored input-bundle entry is incomplete or unbounded")
		}
		if _, duplicate := storedByID[entry.EntryID]; duplicate {
			t.Fatal("stored input-bundle entries contain a duplicate identity")
		}
		storedByID[entry.EntryID] = entryEvidence{
			version:        entry.EntryVersion,
			role:           entry.SemanticRole,
			mediaType:      entry.MediaType,
			digestHex:      entry.DigestHex,
			contentSize:    entry.ContentSize,
			contentRef:     entry.ContentReference,
			classification: entry.Classification,
			audience:       entry.RequiredGrantAudience,
		}
	}
	manifestByID := make(map[string]*runtimev1.ExecutionInputEntryV1, len(manifest.GetEntries()))
	for _, entry := range manifest.GetEntries() {
		content := entry.GetContent()
		storedEntry, exists := storedByID[entry.GetEntryId()]
		if !exists || content == nil ||
			entry.GetImmutableVersion() != storedEntry.version ||
			entry.GetSemanticRole() != storedEntry.role ||
			content.GetImmutableVersion() != storedEntry.version ||
			content.GetMediaType() != storedEntry.mediaType ||
			int64(content.GetByteLength()) != storedEntry.contentSize ||
			hex.EncodeToString(content.GetDigest().GetValue()) != storedEntry.digestHex ||
			content.GetContentId() != storedEntry.contentRef ||
			content.GetClassification() != storedEntry.classification ||
			content.GetRequiredGrantAudience() != storedEntry.audience ||
			!validIssue5681DigestValue(content.GetDigest()) {
			t.Fatal("canonical input-bundle manifest entry does not match stored evidence")
		}
		if _, duplicate := manifestByID[entry.GetEntryId()]; duplicate {
			t.Fatal("canonical input-bundle manifest aliases an entry identity")
		}
		manifestByID[entry.GetEntryId()] = entry
	}
	referencedRoles := make(map[string]struct{}, len(command.Entries))
	for _, reference := range command.Entries {
		entry, exists := manifestByID[reference.EntryID]
		if !exists || entry.GetSemanticRole() != reference.SemanticRole {
			t.Fatal("decoded Redis command reference does not match its exact manifest role")
		}
		if _, duplicate := referencedRoles[reference.SemanticRole]; duplicate {
			t.Fatal("decoded Redis command references a semantic role twice")
		}
		referencedRoles[reference.SemanticRole] = struct{}{}
		if reference.ImmutableVersion != "" &&
			(reference.ImmutableVersion != entry.GetImmutableVersion() ||
				reference.DigestHex != hex.EncodeToString(entry.GetContent().GetDigest().GetValue())) {
			t.Fatal("decoded Redis binding version/digest does not match its manifest entry")
		}
	}
	if len(referencedRoles) != len(storedByID) {
		t.Fatal("stored input bundle contains an entry not referenced by the Redis command")
	}
}

func validIssue5681Digest(digest *runtimev1.DigestV1, value []byte) bool {
	if !validIssue5681DigestValue(digest) {
		return false
	}
	actual := sha256.Sum256(value)
	return bytes.Equal(digest.GetValue(), actual[:])
}

func validIssue5681DigestValue(digest *runtimev1.DigestV1) bool {
	return digest != nil &&
		digest.GetAlgorithm() == runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 &&
		len(digest.GetValue()) == sha256.Size
}

func assertIssue5681BoundedAdmission(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	expectedInputEntries int64,
) {
	t.Helper()
	snapshot := readIssue5681DurableSnapshot(t, ctx, harness, executionID)
	if snapshot.InputManifestBytes <= 0 || snapshot.InputManifestBytes > 64*1024 ||
		snapshot.InputEntryCount != expectedInputEntries ||
		snapshot.InputEntryCount < 2 || snapshot.InputEntryCount > 6 ||
		snapshot.InputEntryBytes <= 0 || snapshot.InputEntryBytes > 6*256*1024 ||
		snapshot.MaxInputEntryBytes <= 0 || snapshot.MaxInputEntryBytes > 256*1024 ||
		snapshot.PreparedEnvelopeBytes <= 0 || snapshot.PreparedEnvelopeBytes > 48*1024 ||
		snapshot.TenantID != strconv.FormatInt(harness.config.projectID, 10) ||
		snapshot.ResourceProjectID != harness.config.projectID ||
		snapshot.ProjectionProjectID != harness.config.projectID ||
		!snapshot.Published || snapshot.AuthorityGranted || snapshot.Retired {
		t.Fatalf("admission crossed a bounded control/configuration contract: %+v", snapshot)
	}
}

func waitForIssue5681Terminal(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
) index5681DurableSnapshot {
	t.Helper()
	var last index5681DurableSnapshot
	err := pollIndexReliability(ctx, 250*time.Millisecond, func() (bool, error) {
		snapshot, err := issue5681DurableSnapshotResult(ctx, harness, executionID)
		if err != nil {
			return false, err
		}
		last = snapshot
		return snapshot.State == "SUCCEEDED" ||
			snapshot.State == "FAILED" ||
			snapshot.State == "CANCELLED", nil
	})
	if err != nil {
		t.Fatalf("wait for issue #5681 terminal settlement: %v last=%+v", err, last)
	}
	return last
}

func readIssue5681DurableSnapshot(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
) index5681DurableSnapshot {
	t.Helper()
	snapshot, err := issue5681DurableSnapshotResult(ctx, harness, executionID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func issue5681DurableSnapshotResult(
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
) (index5681DurableSnapshot, error) {
	if !executionIDPattern.MatchString(executionID) {
		return index5681DurableSnapshot{}, errors.New("invalid execution identity")
	}
	query := fmt.Sprintf(`
	SELECT json_build_object(
	    'state', j.state,
	    'desired_state', j.desired_state,
	    'invocation_state', j.invocation_state,
	    'tenant_id', j.tenant_id,
	    'resource_project_id', j.resource_project_id,
	    'projection_project_id', j.projection_project_id,
	    'workload_identity', COALESCE((
	        SELECT c.workload_identity FROM elitea_runtime.execution_claims AS c
	        WHERE c.execution_id = j.execution_id AND c.generation = j.generation
	        ORDER BY c.claim_attempt DESC LIMIT 1
	    ), ''),
	    'workload_session_id', COALESCE((
	        SELECT c.workload_session_id FROM elitea_runtime.execution_claims AS c
	        WHERE c.execution_id = j.execution_id AND c.generation = j.generation
	        ORDER BY c.claim_attempt DESC LIMIT 1
	    ), ''),
	    'claims', (SELECT count(*) FROM elitea_runtime.execution_claims AS c
               WHERE c.execution_id = j.execution_id AND c.generation = j.generation),
    'max_claim_attempt', COALESCE((SELECT max(c.claim_attempt)
                                  FROM elitea_runtime.execution_claims AS c
                                  WHERE c.execution_id = j.execution_id
                                    AND c.generation = j.generation), 0),
    'released_claims', (SELECT count(*) FROM elitea_runtime.execution_claims AS c
                        WHERE c.execution_id = j.execution_id
                          AND c.generation = j.generation
                          AND c.released_at IS NOT NULL),
    'results', (SELECT count(*) FROM elitea_runtime.index_ingest_results AS i
                WHERE i.execution_id = j.execution_id AND i.generation = j.generation),
    'settlements', (SELECT count(*) FROM elitea_runtime.execution_settlements AS s
                    WHERE s.execution_id = j.execution_id AND s.generation = j.generation),
    'replay_events', (SELECT count(*) FROM elitea_runtime.execution_replay_events AS r
                      WHERE r.execution_id = j.execution_id AND r.generation = j.generation),
    'index_completed_replay', (SELECT count(*) FROM elitea_runtime.execution_replay_events AS r
                               WHERE r.execution_id = j.execution_id
                                 AND r.generation = j.generation
                                 AND r.event_type = 'index.ingest.completed'),
    'terminal_replay_event_seen', EXISTS (
        SELECT 1 FROM elitea_runtime.execution_replay_events AS r
        WHERE r.execution_id = j.execution_id AND r.generation = j.generation
          AND r.event_type = 'index.ingest.completed'
    ),
    'input_manifest_bytes', octet_length(b.manifest_bytes),
    'input_entry_count', (SELECT count(*)
                          FROM elitea_runtime.input_bundle_entries AS e
                          WHERE e.input_bundle_id = b.input_bundle_id),
    'input_entry_bytes', (SELECT COALESCE(sum(octet_length(e.content_bytes)), 0)
                          FROM elitea_runtime.input_bundle_entries AS e
                          WHERE e.input_bundle_id = b.input_bundle_id),
    'max_input_entry_bytes', (SELECT COALESCE(max(octet_length(e.content_bytes)), 0)
                              FROM elitea_runtime.input_bundle_entries AS e
                              WHERE e.input_bundle_id = b.input_bundle_id),
    'prepared_envelope_bytes', COALESCE(octet_length(o.prepared_signed_envelope_bytes), 0),
    'max_replay_event_bytes', COALESCE((SELECT max(octet_length(r.event_bytes))
                                       FROM elitea_runtime.execution_replay_events AS r
                                       WHERE r.execution_id = j.execution_id
                                         AND r.generation = j.generation), 0),
    'total_replay_event_bytes', COALESCE((SELECT sum(octet_length(r.event_bytes))
                                         FROM elitea_runtime.execution_replay_events AS r
                                         WHERE r.execution_id = j.execution_id
                                           AND r.generation = j.generation), 0),
    'node_replay_events', (SELECT count(*)
                           FROM elitea_runtime.execution_replay_events AS r
                           WHERE r.execution_id = j.execution_id
                             AND r.generation = j.generation
                             AND r.event_type = 'execution.node_event'),
    'max_output_inbox_bytes', COALESCE((SELECT max(octet_length(i.payload_bytes))
                                       FROM elitea_runtime.output_inbox AS i
                                       WHERE i.execution_id = j.execution_id
                                         AND i.generation = j.generation), 0),
    'total_output_inbox_bytes', COALESCE((SELECT sum(octet_length(i.payload_bytes))
                                         FROM elitea_runtime.output_inbox AS i
                                         WHERE i.execution_id = j.execution_id
                                           AND i.generation = j.generation), 0),
    'output_inbox_frames', (SELECT count(*) FROM elitea_runtime.output_inbox AS i
                            WHERE i.execution_id = j.execution_id
                              AND i.generation = j.generation),
    'last_node_sequence', COALESCE((
        SELECT r.last_node_sequence
        FROM elitea_runtime.execution_replay_state AS r
        WHERE r.execution_id = j.execution_id AND r.generation = j.generation
    ), 0),
    'terminal_sequence', COALESCE((
        SELECT s.terminal_sequence
        FROM elitea_runtime.execution_settlements AS s
        WHERE s.execution_id = j.execution_id AND s.generation = j.generation
    ), 0),
    'output_identity_bound', EXISTS (
        SELECT 1
        FROM elitea_runtime.output_inbox AS i
        JOIN elitea_runtime.execution_claims AS c
          ON c.claim_id = i.claim_id
         AND c.execution_id = i.execution_id
         AND c.generation = i.generation
         AND c.fence_token = i.fence_token
         AND c.workload_identity = i.workload_identity
         AND c.workload_session_id = i.workload_session_id
         AND c.producer_id = i.producer_id
         AND c.claim_attempt = i.claim_attempt
         AND c.lease_epoch = i.lease_epoch
        JOIN elitea_runtime.execution_settlements AS s
          ON s.execution_id = i.execution_id
         AND s.generation = i.generation
         AND s.claim_id = i.claim_id
         AND s.fence_token = i.fence_token
         AND s.workload_identity = i.workload_identity
         AND s.workload_session_id = i.workload_session_id
         AND s.producer_id = i.producer_id
         AND s.claim_attempt = i.claim_attempt
         AND s.lease_epoch = i.lease_epoch
         AND s.final_logical_output_id = i.logical_output_id
         AND s.terminal_event_id = i.event_id
         AND s.terminal_sequence = i.sequence
         AND s.terminal_payload_digest = i.payload_digest
         AND s.idempotency_key = i.settlement_idempotency_key
         AND s.proposal_id = i.settlement_proposal_id
         AND s.proposal_bytes = i.settlement_proposal_bytes
         AND s.proposal_digest = i.settlement_proposal_digest
        JOIN elitea_runtime.execution_replay_state AS r
          ON r.execution_id = i.execution_id
         AND r.generation = i.generation
         AND i.sequence = r.last_node_sequence + 1
        WHERE i.execution_id = j.execution_id
          AND i.generation = j.generation
          AND i.payload_type = 'INDEX_INGEST_RESULT'
          AND i.projected_at IS NOT NULL
          AND s.committed_at IS NOT NULL
          AND c.released_at IS NOT NULL
          AND c.release_reason = 'SETTLED'
    ),
    'workload_session_active', EXISTS (
        SELECT 1
        FROM elitea_runtime.execution_claims AS c
        JOIN elitea_runtime.workload_sessions AS w
          ON w.workload_session_id = c.workload_session_id
         AND w.workload_identity = c.workload_identity
         AND w.producer_id = c.producer_id
        WHERE c.execution_id = j.execution_id
          AND c.generation = j.generation
          AND w.issued_at <= clock_timestamp()
          AND w.expires_at > clock_timestamp()
          AND w.revoked_at IS NULL
    ),
    'completion_status', COALESCE((SELECT i.completion_status
                                  FROM elitea_runtime.index_ingest_results AS i
                                  WHERE i.execution_id = j.execution_id
                                    AND i.generation = j.generation
                                  LIMIT 1), ''),
    'completion_message', COALESCE((SELECT i.completion_message
                                   FROM elitea_runtime.index_ingest_results AS i
                                   WHERE i.execution_id = j.execution_id
                                     AND i.generation = j.generation
                                   LIMIT 1), ''),
    'published', o.published_at IS NOT NULL,
    'authority_granted', o.authority_granted_at IS NOT NULL,
    'retired', o.retired_at IS NOT NULL,
    'committed_settlement', EXISTS (
        SELECT 1 FROM elitea_runtime.execution_settlements AS s
        WHERE s.execution_id = j.execution_id AND s.generation = j.generation
          AND s.committed_at IS NOT NULL
    )
)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.input_bundles AS b ON b.input_bundle_id = j.input_bundle_id
WHERE j.execution_id = '%s' AND j.capability_id = 'index.ingest.v1'`, executionID)
	output, err := harness.postgres(ctx, query)
	if err != nil {
		return index5681DurableSnapshot{}, err
	}
	if strings.TrimSpace(output) == "" {
		return index5681DurableSnapshot{}, errors.New("execution is not durably visible")
	}
	var snapshot index5681DurableSnapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &snapshot); err != nil {
		return index5681DurableSnapshot{}, fmt.Errorf("decode durable snapshot: %w", err)
	}
	return snapshot, nil
}

func assertIssue5681DurableTerminal(
	t *testing.T,
	snapshot index5681DurableSnapshot,
	projectID int64,
	expectedWorkloadIdentity string,
) {
	t.Helper()
	if snapshot.DesiredState != "RUNNING" ||
		snapshot.InvocationState != "MAY_HAVE_STARTED" ||
		snapshot.TenantID != strconv.FormatInt(projectID, 10) ||
		snapshot.ResourceProjectID != projectID ||
		snapshot.ProjectionProjectID != projectID ||
		snapshot.WorkloadIdentity != expectedWorkloadIdentity ||
		snapshot.WorkloadSessionID == "" ||
		!snapshot.WorkloadSessionActive ||
		snapshot.Claims != 1 ||
		snapshot.MaxClaimAttempt != 1 ||
		snapshot.ReleasedClaims != 1 ||
		snapshot.Results != 1 ||
		snapshot.Settlements != 1 ||
		snapshot.ReplayEvents < 4 ||
		snapshot.ReplayEvents > 65 ||
		snapshot.NodeReplayEvents < 3 ||
		snapshot.NodeReplayEvents > 64 ||
		snapshot.IndexCompletedReplay != 1 ||
		!snapshot.TerminalReplayEventSeen ||
		!snapshot.CommittedSettlement ||
		!snapshot.AuthorityGranted ||
		snapshot.Retired ||
		snapshot.MaxReplayEventBytes <= 0 ||
		snapshot.MaxReplayEventBytes > 48*1024 ||
		snapshot.TotalReplayEventBytes <= 0 ||
		snapshot.TotalReplayEventBytes > 1024*1024 ||
		snapshot.OutputInboxFrames != 1 ||
		snapshot.MaxOutputInboxBytes <= 0 ||
		snapshot.MaxOutputInboxBytes > 256*1024 ||
		snapshot.TotalOutputInboxBytes != snapshot.MaxOutputInboxBytes ||
		snapshot.LastNodeSequence != snapshot.NodeReplayEvents ||
		snapshot.TerminalSequence != snapshot.LastNodeSequence+1 ||
		!snapshot.OutputIdentityBound {
		t.Fatalf("terminal durability/idempotency contract mismatch: %+v", snapshot)
	}
}

func observeIssue5681SSE(
	t *testing.T,
	parent context.Context,
	harness *indexComposeHarness,
	executionID string,
	expectedStreamID string,
	expectedMessageID string,
	forbiddenValues []string,
) (context.CancelFunc, <-chan index5681SSEObservation) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan index5681SSEObservation, 1)
	ready := make(chan error, 1)
	go func() {
		observation, err := collectIssue5681SSE(
			ctx,
			harness,
			executionID,
			expectedStreamID,
			expectedMessageID,
			forbiddenValues,
			0,
			ready,
		)
		if err != nil && !errors.Is(err, context.Canceled) {
			select {
			case ready <- err:
			default:
			}
		}
		done <- observation
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			t.Fatalf("open issue #5681 SSE: %v", err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("issue #5681 SSE did not return response headers")
	}
	return cancel, done
}

func collectIssue5681SSE(
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	expectedStreamID string,
	expectedMessageID string,
	forbiddenValues []string,
	lastEventID int64,
	ready chan<- error,
) (observation index5681SSEObservation, resultErr error) {
	endpoint := fmt.Sprintf(
		"%s/api/v2/executions/%d/%s/events",
		harness.config.baseURL,
		harness.config.projectID,
		executionID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return index5681SSEObservation{}, err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cookie", harness.config.cookie)
	if lastEventID > 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	client := *harness.config.httpClient
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		return index5681SSEObservation{}, err
	}
	defer joinIndexReliabilityCloseError(
		&resultErr,
		response.Body,
		"issue #5681 SSE response",
	)
	if response.StatusCode != http.StatusOK ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return index5681SSEObservation{}, fmt.Errorf(
			"status=%d content-type=%q",
			response.StatusCode,
			response.Header.Get("Content-Type"),
		)
	}
	ready <- nil

	observation = index5681SSEObservation{
		ThinkingIndex:   -1,
		InProgressIndex: -1,
		CompletedIndex:  -1,
	}
	var eventType string
	var eventID string
	var data strings.Builder
	var toolRunID string
	previousEventID := lastEventID
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 128*1024)
	flush := func() bool {
		if eventType == "" {
			eventID = ""
			data.Reset()
			return false
		}
		raw := []byte(data.String())
		cursor, cursorErr := strconv.ParseInt(eventID, 10, 64)
		if cursorErr != nil || cursor <= previousEventID {
			observation.InvalidContract = true
		} else {
			previousEventID = cursor
		}
		observation.EventCount++
		observation.EventIDs = append(observation.EventIDs, cursor)
		sum := sha256.Sum256(raw)
		observation.EventDigests = append(observation.EventDigests, hex.EncodeToString(sum[:]))
		observation.EventTypes = append(observation.EventTypes, eventType)
		observation.MaxDataBytes = max(observation.MaxDataBytes, len(raw))
		observation.TotalDataBytes += len(raw)
		if issue5681UnsafePayload(raw, forbiddenValues) {
			observation.UnsafeDataObserved = true
		}
		switch eventType {
		case "execution.node_event":
			event, err := nodeevent.DecodeCurrentJSON(raw)
			if err != nil {
				observation.InvalidContract = true
				break
			}
			canonical, err := nodeevent.EncodeCurrentJSON(event)
			if err != nil || !bytes.Equal(canonical, raw) ||
				event.GetStreamId() != expectedStreamID ||
				event.GetMessageId() != expectedMessageID ||
				event.GetSioEvent() != "chat_predict" ||
				event.QuestionId != nil ||
				!bytes.Equal(event.GetContent(), []byte("null")) ||
				event.Thinking != nil ||
				!bytes.Equal(event.GetReferences(), []byte("[]")) ||
				event.CreatedAt == nil ||
				!issue5681RFC3339(event.GetCreatedAt()) ||
				event.ParentMessageId != nil ||
				event.AgentName != nil ||
				event.ExecutionGeneration != nil {
				observation.InvalidContract = true
			}
			eventIndex := len(observation.NodeTypes)
			observation.NodeTypes = append(observation.NodeTypes, event.GetType())
			var metadata map[string]any
			decoder := json.NewDecoder(bytes.NewReader(event.GetResponseMetadata()))
			decoder.UseNumber()
			if err := decoder.Decode(&metadata); err != nil {
				observation.InvalidContract = true
				break
			}
			currentToolRunID, _ := metadata["tool_run_id"].(string)
			if currentToolRunID == "" {
				observation.InvalidContract = true
			} else if toolRunID == "" {
				toolRunID = currentToolRunID
				observation.ToolRunID = currentToolRunID
			} else if toolRunID != currentToolRunID {
				observation.InvalidContract = true
			}
			switch event.GetType() {
			case "agent_tool_start":
				startedAt, startedAtOK := metadata["timestamp_start"].(string)
				if !issue5681ExactJSONKeys(metadata,
					"tool_name", "tool_run_id", "timestamp_start", "metadata") ||
					metadata["tool_name"] != "index_data" ||
					metadata["tool_run_id"] != observation.ToolRunID ||
					!startedAtOK ||
					!issue5681RFC3339(startedAt) ||
					startedAt != event.GetCreatedAt() ||
					!issue5681ValidToolMetadata(metadata["metadata"], &observation) {
					observation.InvalidContract = true
				}
				observation.ToolStartedAt = startedAt
			case "agent_thinking_step", "agent_thinking_step_update":
				observation.ThinkingEvents++
				if observation.ThinkingIndex == -1 {
					observation.ThinkingIndex = eventIndex
				}
				if !issue5681ExactJSONKeys(metadata,
					"name", "run_id", "tool_run_id", "metadata", "datetime",
					"message", "tool_name", "toolkit") ||
					metadata["name"] != "thinking_step" ||
					metadata["datetime"] != event.GetCreatedAt() ||
					!issue5681ValidCommonNodeMetadata(metadata, &observation) {
					observation.InvalidContract = true
				}
				message, messageOK := metadata["message"].(string)
				toolName, toolNameOK := metadata["tool_name"].(string)
				toolkit, toolkitOK := metadata["toolkit"].(string)
				if !messageOK || !toolNameOK || !toolkitOK {
					observation.InvalidContract = true
				}
				observation.ThinkingMessages = append(observation.ThinkingMessages, message)
				observation.ThinkingToolNames = append(observation.ThinkingToolNames, toolName)
				observation.ThinkingToolkits = append(observation.ThinkingToolkits, toolkit)
			case "agent_index_data_status":
				state, _ := metadata["state"].(string)
				observation.StatusStates = append(observation.StatusStates, state)
				if !issue5681ExactJSONKeys(metadata,
					"name", "run_id", "tool_run_id", "metadata", "datetime",
					"id", "index_name", "state", "error", "reindex", "indexed",
					"updated", "created_at", "updated_on", "toolkit_id",
					"task_id", "initiator", "project_id", "user_id") ||
					metadata["name"] != "index_data_status" ||
					metadata["task_id"] != executionID ||
					metadata["datetime"] != event.GetCreatedAt() ||
					!issue5681JSONIntegerEquals(metadata["project_id"], harness.config.projectID) ||
					!issue5681JSONIntegerEquals(metadata["toolkit_id"], harness.config.toolkitID) ||
					!issue5681ValidCommonNodeMetadata(metadata, &observation) {
					observation.InvalidContract = true
				}
				statusIndexName, indexNameOK := metadata["index_name"].(string)
				indexed, indexedOK := issue5681JSONInteger(metadata["indexed"])
				updated, updatedOK := issue5681JSONInteger(metadata["updated"])
				createdAt, createdAtOK := metadata["created_at"].(string)
				updatedOn, updatedOnOK := metadata["updated_on"].(string)
				if !indexNameOK || !indexedOK || !updatedOK ||
					!createdAtOK || !updatedOnOK ||
					!issue5681RFC3339(createdAt) || !issue5681RFC3339(updatedOn) ||
					metadata["error"] != nil || metadata["reindex"] != false {
					observation.InvalidContract = true
				}
				observation.StatusIndexNames = append(observation.StatusIndexNames, statusIndexName)
				observation.StatusIndexed = append(observation.StatusIndexed, indexed)
				observation.StatusUpdated = append(observation.StatusUpdated, updated)
				metaID, metaIDOK := metadata["id"].(string)
				if !metaIDOK || metaID == "" {
					observation.InvalidContract = true
				} else if observation.StatusMetaID == "" {
					observation.StatusMetaID = metaID
				} else if observation.StatusMetaID != metaID {
					observation.InvalidContract = true
				}
				switch state {
				case "in_progress":
					if observation.InProgressIndex != -1 {
						observation.InvalidContract = true
					}
					observation.InProgressIndex = eventIndex
				case "completed":
					if observation.CompletedIndex != -1 {
						observation.InvalidContract = true
					}
					observation.CompletedIndex = eventIndex
				default:
					observation.InvalidContract = true
				}
			case "agent_tool_end":
				finishedAt, finishedAtOK := metadata["timestamp_finish"].(string)
				if !issue5681ExactJSONKeys(metadata,
					"tool_name", "tool_run_id", "finish_reason",
					"timestamp_start", "timestamp_finish") ||
					metadata["tool_name"] != "index_data" ||
					metadata["tool_run_id"] != observation.ToolRunID ||
					metadata["finish_reason"] != "stop" ||
					metadata["timestamp_start"] != observation.ToolStartedAt ||
					!finishedAtOK || !issue5681RFC3339(finishedAt) ||
					finishedAt != event.GetCreatedAt() {
					observation.InvalidContract = true
				}
			default:
				observation.InvalidContract = true
			}
		case "index.ingest.completed":
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			var terminal struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			}
			if err := decoder.Decode(&terminal); err != nil ||
				decoder.Decode(new(any)) != io.EOF {
				observation.InvalidContract = true
			}
			canonical, err := json.Marshal(terminal)
			if err != nil || !bytes.Equal(canonical, raw) {
				observation.InvalidContract = true
			}
			observation.TerminalEvents++
			observation.TerminalSeen = true
			observation.TerminalStatus = terminal.Status
			observation.TerminalMessage = terminal.Message
		default:
			observation.InvalidContract = true
		}
		eventType = ""
		eventID = ""
		data.Reset()
		return observation.TerminalSeen
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if flush() {
				return observation, nil
			}
		}
	}
	flush()
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return observation, err
	}
	return observation, ctx.Err()
}

func issue5681JSONIntegerEquals(value any, expected int64) bool {
	actual, ok := issue5681JSONInteger(value)
	return ok && actual == expected
}

func issue5681JSONInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	actual, err := strconv.ParseInt(number.String(), 10, 64)
	return actual, err == nil
}

func issue5681ExactJSONKeys(value map[string]any, expected ...string) bool {
	if len(value) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, exists := value[key]; !exists {
			return false
		}
	}
	return true
}

func issue5681ValidToolMetadata(
	value any,
	observation *index5681SSEObservation,
) bool {
	metadata, ok := value.(map[string]any)
	if !ok || !issue5681ExactJSONKeys(
		metadata,
		"initiator", "tool_name", "display_name",
	) || metadata["initiator"] != "user" || metadata["tool_name"] != "index_data" {
		return false
	}
	displayName, ok := metadata["display_name"].(string)
	if !ok || displayName == "" || len(displayName) > 256 {
		return false
	}
	if observation.ToolkitDisplayName == "" {
		observation.ToolkitDisplayName = displayName
	}
	return observation.ToolkitDisplayName == displayName
}

func issue5681ValidCommonNodeMetadata(
	metadata map[string]any,
	observation *index5681SSEObservation,
) bool {
	runID, runOK := metadata["run_id"].(string)
	toolRunID, toolRunOK := metadata["tool_run_id"].(string)
	datetime, datetimeOK := metadata["datetime"].(string)
	if !runOK || !toolRunOK || runID == "" || runID != toolRunID ||
		toolRunID != observation.ToolRunID || !datetimeOK {
		return false
	}
	if !issue5681RFC3339(datetime) {
		return false
	}
	return issue5681ValidToolMetadata(metadata["metadata"], observation)
}

func issue5681RFC3339(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func issue5681UnsafePayload(raw []byte, forbiddenValues []string) bool {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{
		"authorization",
		"bearer ",
		"api_key",
		"api-key",
		"access_token",
		"auth_token",
		"private_token",
		"password",
		"client_secret",
		"credential",
		"secret",
		"toolkit_config",
		"tool_params",
		"chunking_config",
		"runtime_config",
		"settings",
		"llm_configuration",
		"mcp_tokens",
		"http://",
		"https://",
		"postgres://",
		"postgresql://",
		"redis://",
		`"url"`,
		`"path"`,
		"/run/",
		"/data/",
		"data:image/",
		"base64,",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	for _, value := range forbiddenValues {
		if value != "" && strings.Contains(lower, strings.ToLower(value)) {
			return true
		}
	}
	for _, field := range strings.FieldsFunc(lower, func(character rune) bool {
		return (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') &&
			character != '+' && character != '/' && character != '='
	}) {
		if len(field) > 4096 {
			return true
		}
	}
	return false
}

func finishIssue5681SSE(
	t *testing.T,
	cancel context.CancelFunc,
	done <-chan index5681SSEObservation,
) index5681SSEObservation {
	t.Helper()
	select {
	case result := <-done:
		cancel()
		return result
	case <-time.After(10 * time.Second):
		cancel()
		select {
		case result := <-done:
			return result
		case <-time.After(5 * time.Second):
			t.Fatal("issue #5681 SSE did not return terminal replay")
			return index5681SSEObservation{}
		}
	}
}

func assertIssue5681SSE(
	t *testing.T,
	observation index5681SSEObservation,
	durable index5681DurableSnapshot,
	indexName string,
) {
	t.Helper()
	expectedCompletion := "Successfully indexed 1 documents (12 chunks)."
	title := "Issue 5681 production-scale image corpus"
	expectedNodeTypes := []string{
		"agent_tool_start",
		"agent_thinking_step",
		"agent_index_data_status",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_thinking_step",
		"agent_index_data_status",
		"agent_tool_end",
	}
	expectedMessages := []string{
		fmt.Sprintf("There is no existing index_meta for collection '%s'. Initializing it.", indexName),
		fmt.Sprintf("Indexing data into collection with suffix '%s'. It can take some time...", indexName),
		"Loading the documents to index.",
		"Base documents were pre-loaded. Search for possible document duplicates and remove them from the indexing list...",
		"Duplicates were removed. Processing documents to collect dependencies and prepare them for indexing...",
		"Base documents are ready for indexing. 1 base documents in total to index.",
		"Verification of documents to index started",
		"Retrieving already indexed data from PGVector vectorstore",
		fmt.Sprintf("Processing document #1: '%s'.", title),
		fmt.Sprintf("Dependent documents for '%s' were processed. Applying chunking tool 'default' if specified and preparing documents for indexing...", title),
		fmt.Sprintf("Collecting the dependencies for document '%s' (ID: 'page-5681') to collect dependencies if any...", title),
		fmt.Sprintf("Indexed document #1 '%s' out of 1 (with 12 chunks).", title),
		"12 documents have been indexed. Continuing...",
	}
	expectedToolNames := []string{
		"index_data",
		"tool_progress",
		"tool_progress",
		"tool_progress",
		"tool_progress",
		"tool_progress",
		"index_documents",
		"get_indexed_data",
		"tool_progress",
		"tool_progress",
		"tool_progress",
		"tool_progress",
		"tool_progress",
	}
	expectedToolkits := make([]string, len(expectedMessages))
	for index := range expectedToolkits {
		expectedToolkits[index] = "ConfluenceAPIWrapper"
	}
	if observation.InvalidContract ||
		observation.UnsafeDataObserved ||
		!observation.TerminalSeen ||
		observation.TerminalEvents != 1 ||
		observation.TerminalStatus != "ok" ||
		observation.TerminalMessage != expectedCompletion ||
		observation.EventCount != int(durable.ReplayEvents) ||
		len(observation.NodeTypes) != int(durable.NodeReplayEvents) ||
		observation.TotalDataBytes != int(durable.TotalReplayEventBytes) ||
		observation.MaxDataBytes <= 0 ||
		observation.MaxDataBytes > nodeevent.MaxCurrentJSONBytes ||
		observation.TotalDataBytes > 1024*1024 ||
		!issue5681StringSlicesEqual(observation.NodeTypes, expectedNodeTypes) ||
		!issue5681StringSlicesEqual(observation.ThinkingMessages, expectedMessages) ||
		!issue5681StringSlicesEqual(observation.ThinkingToolNames, expectedToolNames) ||
		!issue5681StringSlicesEqual(observation.ThinkingToolkits, expectedToolkits) ||
		observation.ThinkingEvents != len(expectedMessages) ||
		observation.ThinkingIndex != 1 ||
		observation.InProgressIndex != 2 ||
		observation.CompletedIndex != len(expectedNodeTypes)-2 ||
		observation.ToolRunID == "" ||
		observation.StatusMetaID == "" ||
		observation.ToolkitDisplayName == "" ||
		len(observation.StatusStates) != 2 ||
		observation.StatusStates[0] != "in_progress" ||
		observation.StatusStates[1] != "completed" ||
		!issue5681StringSlicesEqual(
			observation.StatusIndexNames,
			[]string{indexName, indexName},
		) ||
		!issue5681Int64SlicesEqual(observation.StatusIndexed, []int64{0, 1}) ||
		!issue5681Int64SlicesEqual(observation.StatusUpdated, []int64{0, 12}) ||
		observation.EventTypes[len(observation.EventTypes)-1] != "index.ingest.completed" {
		t.Fatalf("SSE did not preserve the exact bounded current indexing contract: %+v", observation)
	}
	for index, eventType := range observation.EventTypes {
		if index == len(observation.EventTypes)-1 {
			continue
		}
		if eventType != "execution.node_event" {
			t.Fatalf("unexpected non-terminal SSE event %q at position %d", eventType, index)
		}
	}
}

func assertIssue5681SSEReconnect(
	t *testing.T,
	parent context.Context,
	harness *indexComposeHarness,
	executionID string,
	expectedStreamID string,
	expectedMessageID string,
	forbiddenValues []string,
	original index5681SSEObservation,
) {
	t.Helper()
	if len(original.EventIDs) < 4 ||
		len(original.EventIDs) != len(original.EventDigests) ||
		len(original.EventIDs) != len(original.EventTypes) {
		t.Fatal("initial SSE replay lacks a durable reconnect cursor")
	}
	cursorIndex := len(original.EventIDs)/2 - 1
	cursor := original.EventIDs[cursorIndex]
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	replayed, err := collectIssue5681SSE(
		ctx,
		harness,
		executionID,
		expectedStreamID,
		expectedMessageID,
		forbiddenValues,
		cursor,
		ready,
	)
	if err != nil {
		t.Fatalf("reconnect issue #5681 SSE from durable cursor: %v", err)
	}
	select {
	case readyErr := <-ready:
		if readyErr != nil {
			t.Fatalf("open issue #5681 SSE reconnect: %v", readyErr)
		}
	default:
		t.Fatal("SSE reconnect did not confirm response headers")
	}
	expectedIDs := original.EventIDs[cursorIndex+1:]
	expectedDigests := original.EventDigests[cursorIndex+1:]
	expectedTypes := original.EventTypes[cursorIndex+1:]
	if replayed.InvalidContract || replayed.UnsafeDataObserved ||
		!replayed.TerminalSeen ||
		!issue5681Int64SlicesEqual(replayed.EventIDs, expectedIDs) ||
		!issue5681StringSlicesEqual(replayed.EventDigests, expectedDigests) ||
		!issue5681StringSlicesEqual(replayed.EventTypes, expectedTypes) {
		t.Fatal("Last-Event-ID reconnect did not return the exact durable replay suffix")
	}
}

func issue5681StringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func issue5681Int64SlicesEqual(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func waitForIssue5681Receipt(
	t *testing.T,
	ctx context.Context,
	baseURL string,
) index5681FixtureReceipt {
	t.Helper()
	var last index5681FixtureReceipt
	err := pollIndexReliability(ctx, 250*time.Millisecond, func() (bool, error) {
		receipt, err := issue5681ReceiptResult(ctx, baseURL)
		if err != nil {
			return false, err
		}
		last = receipt
		return receipt.SourceCompletedBytes >= index5681CurrentSourceBytes &&
			receipt.ChatRequests >= index5681CurrentVisionCalls &&
			receipt.EmbeddingRequests >= 1, nil
	})
	if err != nil {
		t.Fatalf("wait for issue #5681 source/model receipt: %v last=%+v", err, last)
	}
	return last
}

func readIssue5681Receipt(
	t *testing.T,
	ctx context.Context,
	baseURL string,
) index5681FixtureReceipt {
	t.Helper()
	receipt, err := issue5681ReceiptResult(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func issue5681ReceiptResult(
	ctx context.Context,
	baseURL string,
) (receipt index5681FixtureReceipt, resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/__elitea_issue_5681/receipt", nil)
	if err != nil {
		return index5681FixtureReceipt{}, err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return index5681FixtureReceipt{}, err
	}
	defer joinIndexReliabilityCloseError(
		&resultErr,
		response.Body,
		"issue #5681 receipt response",
	)
	if response.StatusCode != http.StatusOK {
		return index5681FixtureReceipt{}, fmt.Errorf("receipt status=%d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return index5681FixtureReceipt{}, err
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		return index5681FixtureReceipt{}, fmt.Errorf("decode receipt: %w", err)
	}
	return receipt, nil
}

func assertIssue5681Receipt(t *testing.T, receipt index5681FixtureReceipt) {
	t.Helper()
	if receipt.Schema != index5681ReceiptSchema ||
		receipt.Profile != index5681FixtureProfile ||
		receipt.DeclaredSourcePayloadBytes != index5681FixtureBytes ||
		len(receipt.SmallImageSHA256) != 64 ||
		len(receipt.LargeImageSHA256) != 64 ||
		receipt.SourceCompletedBytes != index5681CurrentSourceBytes ||
		receipt.ChatRequests != index5681CurrentVisionCalls ||
		receipt.MaxChatRequestBytes <= index5681LargeImageBytes ||
		receipt.EmbeddingRequests < 1 ||
		receipt.RejectedSourceRequests != 0 ||
		receipt.RejectedModelRequests != 0 ||
		receipt.RejectedProxyRequests != 0 {
		t.Fatalf("source/model receipt does not prove the exact issue #5681 profile: %+v", receipt)
	}
	if len(receipt.SourceCompletedRequests) != 11 ||
		receipt.SourceCompletedRequests["diagram-32mib.png"] != 2 {
		t.Fatalf("source receipt does not contain each exact current-baseline image pass: %+v", receipt.SourceCompletedRequests)
	}
	for ordinal := 0; ordinal < 10; ordinal++ {
		name := fmt.Sprintf("diagram-%02d.png", ordinal)
		if receipt.SourceCompletedRequests[name] != 2 {
			t.Fatalf("source receipt %q count=%d, want 2", name, receipt.SourceCompletedRequests[name])
		}
	}
}

func removeIssue5681SyntheticConsumer(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
) {
	t.Helper()
	const script = `
local removed = redis.call('XGROUP', 'DELCONSUMER', KEYS[1], ARGV[1], ARGV[2])
local consumers = redis.call('XINFO', 'CONSUMERS', KEYS[1], ARGV[1])
local remaining = false
for _, consumer in ipairs(consumers) do
  for i = 1, #consumer, 2 do
    if consumer[i] == 'name' and consumer[i + 1] == ARGV[2] then remaining = true end
  end
end
return cjson.encode({removed_pending = removed, remaining = remaining})`
	output, err := harness.redis(
		ctx,
		"indexer-worker",
		"--raw", "EVAL", script, "1",
		indexCommandStream, indexConsumerGroup, syntheticConsumer,
	)
	if err != nil {
		t.Fatalf("remove synthetic Redis consumer: %v", err)
	}
	var result struct {
		RemovedPending int64 `json:"removed_pending"`
		Remaining      bool  `json:"remaining"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &result) != nil ||
		result.RemovedPending != 0 || result.Remaining {
		t.Fatalf("synthetic Redis consumer was not cleanly removed: %+v", result)
	}
}

type index5681MetaItem struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata"`
	Stale    bool           `json:"stale"`
}

func assertIssue5681PgVectorOutcome(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	indexName string,
) {
	t.Helper()
	items, err := listIssue5681IndexMeta(ctx, harness)
	if err != nil {
		t.Fatal(err)
	}
	var matches []index5681MetaItem
	for _, item := range items {
		if item.Metadata["collection"] == indexName {
			matches = append(matches, item)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("public PgVector metadata contains %d rows for exact collection %q", len(matches), indexName)
	}
	item := matches[0]
	if item.ID == "" || len(item.ID) > 256 || item.Stale ||
		item.Metadata["state"] != "completed" ||
		!issue5681JSONIntegerEquals(item.Metadata["indexed"], 1) ||
		!issue5681JSONIntegerEquals(item.Metadata["updated"], 0) {
		t.Fatal("public PgVector metadata does not prove the exact completed business result")
	}
}

func listIssue5681IndexMeta(
	ctx context.Context,
	harness *indexComposeHarness,
) (items []index5681MetaItem, resultErr error) {
	endpoint := fmt.Sprintf(
		"%s/api/v2/elitea_core/index_meta/prompt_lib/%d/%d",
		harness.config.baseURL,
		harness.config.projectID,
		harness.config.toolkitID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Cookie", harness.config.cookie)
	response, err := harness.config.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read public index metadata: %w", err)
	}
	defer joinIndexReliabilityCloseError(
		&resultErr,
		response.Body,
		"public index metadata response",
	)
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read public index metadata: status=%d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil || len(body) > 8*1024*1024 {
		return nil, errors.New("public index metadata exceeded the test response bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&items); err != nil || len(items) > 1000 {
		return nil, errors.New("decode bounded public index metadata")
	}
	return items, nil
}

func cleanupIssue5681Index(
	ctx context.Context,
	harness *indexComposeHarness,
	indexName string,
) error {
	items, err := listIssue5681IndexMeta(ctx, harness)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Metadata["collection"] != indexName {
			continue
		}
		if item.ID == "" || len(item.ID) > 256 ||
			url.PathEscape(item.ID) != item.ID {
			return errors.New("unsafe public index metadata identity")
		}
		endpoint := fmt.Sprintf(
			"%s/api/v2/elitea_core/index_meta/prompt_lib/%d/%d/%s",
			harness.config.baseURL,
			harness.config.projectID,
			harness.config.toolkitID,
			url.PathEscape(item.ID),
		)
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
		if requestErr != nil {
			return requestErr
		}
		request.Header.Set("Cookie", harness.config.cookie)
		response, requestErr := harness.config.httpClient.Do(request)
		if requestErr != nil {
			return fmt.Errorf("delete public index metadata: %w", requestErr)
		}
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK &&
			response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("delete public index metadata: status=%d", response.StatusCode)
		}
		if closeErr != nil {
			return fmt.Errorf("close public index delete response: %w", closeErr)
		}
	}
	remaining, err := listIssue5681IndexMeta(ctx, harness)
	if err != nil {
		return err
	}
	for _, item := range remaining {
		if item.Metadata["collection"] == indexName {
			return errors.New("public index delete left the collection visible")
		}
	}
	return nil
}

func equalIssue5681Receipts(left, right index5681FixtureReceipt) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func waitForIssue5681CancelledStability(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	expected indexJobSnapshot,
	fixtureBaseURL string,
	expectedReceipt index5681FixtureReceipt,
	canary string,
) {
	t.Helper()
	stable := 0
	err := pollIndexReliability(ctx, 500*time.Millisecond, func() (bool, error) {
		job, err := harness.jobSnapshot(ctx, executionID)
		if err != nil {
			return false, err
		}
		if job != expected {
			return false, fmt.Errorf("cancelled execution changed after worker restart")
		}
		durable, err := issue5681DurableSnapshotResult(ctx, harness, executionID)
		if err != nil {
			return false, err
		}
		if durable.State != "CANCELLED" ||
			durable.DesiredState != "CANCELLED" ||
			durable.Claims != 0 ||
			durable.Results != 0 ||
			durable.Settlements != 0 ||
			durable.OutputInboxFrames != 0 ||
			durable.AuthorityGranted ||
			!durable.Retired {
			return false, errors.New("cancelled execution crossed worker authority after restart")
		}
		redisSnapshot, err := harness.redisReferenceResult(ctx, canary)
		if err != nil {
			return false, err
		}
		pending, err := harness.pendingEntriesResult(ctx)
		if err != nil {
			return false, err
		}
		receipt, err := issue5681ReceiptResult(ctx, fixtureBaseURL)
		if err != nil {
			return false, err
		}
		if redisSnapshot.Length != 0 ||
			redisSnapshot.Mappings != 0 ||
			len(pending) != 0 ||
			!equalIssue5681Receipts(receipt, expectedReceipt) {
			return false, errors.New("cancelled execution produced post-restart side effects")
		}
		stable++
		return stable >= 6, nil
	})
	if err != nil {
		t.Fatalf("post-cancellation worker restart stability: %v", err)
	}
}

func waitForIssue5681StableRestart(
	t *testing.T,
	ctx context.Context,
	harness *indexComposeHarness,
	executionID string,
	expected index5681DurableSnapshot,
) {
	t.Helper()
	stable := 0
	err := pollIndexReliability(ctx, 500*time.Millisecond, func() (bool, error) {
		snapshot, err := issue5681DurableSnapshotResult(ctx, harness, executionID)
		if err != nil {
			return false, err
		}
		if snapshot != expected {
			return false, fmt.Errorf("durable terminal snapshot changed: got=%+v want=%+v", snapshot, expected)
		}
		stable++
		return stable >= 6, nil
	})
	if err != nil {
		t.Fatalf("post-terminal restart idempotency: %v", err)
	}
}

func TestIssue5681FixtureProfileIsExactlySixtyTwoMiB(t *testing.T) {
	if got := 10*(3<<20) + (32 << 20); got != int(index5681FixtureBytes) {
		t.Fatalf("issue #5681 profile bytes=%d, want %d", got, index5681FixtureBytes)
	}
	if index5681LargeImageBytes <= 0 || index5681LargeImageBytes >= index5681FixtureBytes {
		t.Fatal("issue #5681 large-image boundary is malformed")
	}
}

func TestIssue5681PayloadSafetyRejectsBulkAndPrivateMarkers(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"response_metadata":{"authorization":"redacted"}}`),
		[]byte(`{"response_metadata":{"payload":"data:image/png;base64,AAAA"}}`),
		[]byte(`{"response_metadata":{"message":"private-digest"}}`),
		[]byte(`{"response_metadata":{"toolkit_config":{}}}`),
		[]byte(`{"response_metadata":{"url":"https://source.invalid"}}`),
		[]byte(`{"response_metadata":{"access_token":"redacted"}}`),
		[]byte(`{"response_metadata":{"credential":"redacted"}}`),
		[]byte(`{"response_metadata":{"tool_params":{}}}`),
		[]byte(`{"response_metadata":{"chunking_config":{}}}`),
		[]byte(`{"response_metadata":{"settings":{}}}`),
		[]byte(`{"response_metadata":{"path":"/data/private"}}`),
	} {
		if !issue5681UnsafePayload(raw, []string{"private-digest"}) {
			t.Fatalf("unsafe issue #5681 payload was accepted: %s", raw)
		}
	}
	if issue5681UnsafePayload(
		[]byte(`{"type":"agent_thinking_step","response_metadata":{"message":"20 files processed"}}`),
		nil,
	) {
		t.Fatal("bounded safe issue #5681 progress was rejected")
	}
}

func TestIssue5681DigestValidatesExactBytes(t *testing.T) {
	value := []byte("bounded-reference")
	sum := sha256.Sum256(value)
	digest := &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), sum[:]...),
	}
	if !validIssue5681Digest(digest, value) {
		t.Fatal("exact issue #5681 SHA-256 digest was rejected")
	}
	if validIssue5681Digest(digest, []byte("different")) {
		t.Fatal("issue #5681 digest accepted different bytes")
	}
}

func TestIssue5681CleanupManifestIsOwnerOnlyAndRoundTripsSafeIDs(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "cleanup.jsonl")
	t.Setenv(index5681CleanupManifestEnv, path)
	first := index5681CleanupResource{
		ExecutionID: "0123456789abcdef0123456789abcdef",
		ProjectID:   7,
		ToolkitID:   9,
		IndexName:   "rel-5681-first",
	}
	second := index5681CleanupResource{
		ExecutionID: "abcdef0123456789abcdef0123456789",
		ProjectID:   7,
		ToolkitID:   9,
		IndexName:   "rel-5681-second",
	}
	appendIssue5681CleanupResource(t, first)
	appendIssue5681CleanupResource(t, second)
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("cleanup manifest is not owner-only")
	}
	resources, actualPath := readIssue5681CleanupResources(t, indexReliabilityConfig{
		projectID: 7,
		toolkitID: 9,
	})
	if actualPath != path || len(resources) != 2 ||
		resources[0] != first || resources[1] != second {
		t.Fatal("cleanup manifest did not preserve exact safe resource identities")
	}
}

func TestHybridNestedApplicationReferenceRouterIsBounded(t *testing.T) {
	routePath := filepath.Join(
		findRepositoryRoot(t),
		"deploy",
		"centry-hybrid",
		"traefik",
		"index-routes.yml",
	)
	routeBytes, err := os.ReadFile(routePath)
	if err != nil {
		t.Fatalf("read hybrid gateway route %s: %v", routePath, err)
	}
	requireNestedApplicationReferenceRouter(t, string(routeBytes))
}

// TestHybridArtifactRouterIsBoundedToGoMain reads the artifact plane's ONE
// router.
//
// It used to read `runtime-worker-current-artifacts`: a Host(`elitea-gateway`)
// guarded router that sent the worker's S3 calls to pylon, while browser
// artifact traffic fell through base.yml's catch-all to pylon as well. Issue
// 337 replaced both doors with a single `go-artifacts` router at priority 90
// pointing at elitea-main, and deploy/ARTIFACT_CUTOVER.md records the flip.
//
// The assertions move with it rather than go away. The bound this gate holds is
// the same bound as before — the router names the artifact paths and no others,
// and it names one service — and it gains the two facts the cutover created:
// the Host guard is deliberately absent, because a browser must reach the same
// door as the worker, and no OTHER router in this file claims an artifact path,
// because "one owner" is the property the cutover bought.
func TestHybridArtifactRouterIsBoundedToGoMain(t *testing.T) {
	routePath := filepath.Join(
		findRepositoryRoot(t),
		"deploy",
		"centry-hybrid",
		"traefik",
		"index-routes.yml",
	)
	routeBytes, err := os.ReadFile(routePath)
	if err != nil {
		t.Fatalf("read hybrid gateway route %s: %v", routePath, err)
	}
	requireArtifactRouter(t, string(routeBytes))
}

func requireArtifactRouter(t *testing.T, route string) {
	t.Helper()
	if strings.Contains(route, "runtime-worker-current-artifacts:") {
		t.Fatal("the retired current-artifact worker router is back beside the Go artifact router; two doors to one object store is the state issue 337 removed")
	}
	router := hybridGatewayRouterBlock(route, "go-artifacts")
	if router == "" {
		t.Fatal("gateway route lacks the go-artifacts router")
	}
	for _, required := range []string{
		"PathRegexp(`^/api/v2/artifacts/buckets/[1-9][0-9]*(/[^/]+)?$`)",
		"PathRegexp(`^/api/v2/artifacts/objects/[1-9][0-9]*/[^/]+(/.*)?$`)",
		"PathRegexp(`^/api/v2/artifacts/grants/[1-9][0-9]*/.*$`)",
		// NOT under /api/v2, on purpose: the pinned SDK appends
		// "/artifacts/s3" to a platform origin that carries no path, so the
		// request arrives at the root.
		"PathRegexp(`^/artifacts/s3/[^/]+(/.*)?$`)",
		"Method(`GET`)",
		"Method(`POST`)",
		"Method(`PUT`)",
		"Method(`PATCH`)",
		"Method(`DELETE`)",
		"Method(`HEAD`)",
		// Priority 90 is the band of the other browser-reachable Go routers.
		// 85 is the band for the worker-only routers that keep a Host guard,
		// and this one has none to justify sitting there.
		"priority: 90",
		"entryPoints: [websecure]",
		"tls: {}",
		"middlewares:\n        - strip-caller-auth-context\n        - normalize-runtime-public-authority\n        - go-main-forward-auth",
		"service: elitea-main",
	} {
		if !strings.Contains(router, required) {
			t.Fatalf("artifact router has an unexpected semantic contract: %q", required)
		}
	}
	for _, forbidden := range []string{
		// A prefix match would swallow paths this router does not serve.
		"PathPrefix(",
		// THE ABSENT HOST GUARD IS THE DESIGN. The worker-only routers carry
		// Host(`elitea-gateway`); this router serves the browser as well, and
		// restoring the guard would send browser artifact traffic back through
		// base.yml's catch-all to a service that no longer owns the store.
		"Host(`elitea-gateway`)",
		"service: current-main",
	} {
		if strings.Contains(router, forbidden) {
			t.Fatalf("artifact router does not hold the cutover contract: %q must not appear", forbidden)
		}
	}
	for _, name := range hybridGatewayRouterNames(route) {
		if name == "go-artifacts" {
			continue
		}
		if strings.Contains(hybridGatewayRouterBlock(route, name), "artifacts/") {
			t.Fatalf("router %q also claims an artifact path; the artifact plane has ONE owner (issue 337)", name)
		}
	}
}

// hybridGatewayRouterNames lists the routers this file declares, in file order.
// A router key sits at exactly four spaces of indentation; everything inside
// its block is indented deeper.
func hybridGatewayRouterNames(route string) []string {
	var names []string
	for _, line := range strings.Split(route, "\n") {
		if !strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "     ") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimSpace(line), ":")
		if key == strings.TrimSpace(line) || strings.HasPrefix(key, "#") {
			continue
		}
		names = append(names, key)
	}
	return names
}

// hybridGatewayRouterBlock returns one router's YAML block, or "" when the file
// declares no router by that name.
//
// IT ENDS THE BLOCK BY INDENTATION, not by naming the router that follows. The
// previous version sliced up to the literal "runtime-worker-current-artifacts",
// so deleting that router in the artifact cutover silently extended the
// nested-application block to the end of the file: the assertions then read a
// neighbour's rule and reported a contract violation in a router nobody had
// touched.
func hybridGatewayRouterBlock(route, name string) string {
	lines := strings.Split(route, "\n")
	start := -1
	for i, line := range lines {
		if line == "    "+name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if !strings.HasPrefix(lines[i], "     ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n") + "\n"
}

func requireNestedApplicationReferenceRouter(t *testing.T, route string) {
	t.Helper()
	applicationRouter := hybridGatewayRouterBlock(
		route,
		"runtime-worker-current-application-reference",
	)
	if applicationRouter == "" {
		t.Fatal("gateway route lacks the nested-application reference router")
	}
	for _, required := range []string{
		"Host(`elitea-gateway`)",
		"PathRegexp(`^/api/v2/elitea_core/application/prompt_lib/[1-9][0-9]*/[1-9][0-9]*$`)",
		"Method(`GET`)",
		"PathRegexp(`^/api/v2/elitea_core/version/prompt_lib/[1-9][0-9]*/[1-9][0-9]*/[1-9][0-9]*$`)",
		"Method(`PATCH`)",
		"priority: 85",
		"entryPoints: [websecure]",
		"tls: {}",
		"middlewares:\n        - strip-caller-auth-context\n        - normalize-runtime-public-authority\n        - go-main-forward-auth",
		"service: current-main",
	} {
		if strings.Count(applicationRouter, required) != 1 {
			t.Fatal("nested-application reference router has an unexpected semantic contract")
		}
	}
	if strings.Contains(applicationRouter, "PathPrefix(") ||
		strings.Contains(applicationRouter, "Method(`POST`)") ||
		strings.Contains(applicationRouter, "Method(`PUT`)") ||
		strings.Contains(applicationRouter, "Method(`DELETE`)") {
		t.Fatal("nested-application reference router is broader than the SDK read contract")
	}
}
