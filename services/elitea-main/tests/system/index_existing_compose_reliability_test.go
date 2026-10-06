package system_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	indexReliabilityOptIn = "ELITEA_INDEX_RELIABILITY_SYSTEM_TEST"
	indexCommandStream    = "commands.v1.index.ingest.indexing.shared.2.0"
	indexConsumer         = "elitea-indexer-worker-v2"
	indexWorkerService    = "elitea-indexer-worker"
	syntheticConsumer     = "reliability-crashed-consumer"
)

var (
	executionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	redisEntryPattern  = regexp.MustCompile(`^[0-9]+-[0-9]+$`)
)

type indexReliabilityConfig struct {
	centryDir  string
	runtimeDir string
	// composeProject is optional for the inherited local reliability test.
	// Production-scale gates require it so every lifecycle operation targets
	// one explicitly disposable Compose namespace.
	composeProject string
	baseURL        string
	projectID      int64
	cookie         string
	startBody      map[string]any
	toolkitID      int64
	httpClient     *http.Client
	composeEnv     []string
	timeout        time.Duration
}

type indexJobSnapshot struct {
	State            string `json:"state"`
	DesiredState     string `json:"desired_state"`
	TerminalError    string `json:"terminal_error"`
	Claims           int64  `json:"claims"`
	MaxClaimAttempt  int64  `json:"max_claim_attempt"`
	ReplayEvents     int64  `json:"replay_events"`
	Published        bool   `json:"published"`
	AuthorityGranted bool   `json:"authority_granted"`
	Retired          bool   `json:"retired"`
}

type indexRedisReferenceSnapshot struct {
	Length         int64 `json:"length"`
	Mappings       int64 `json:"mappings"`
	EntryCount     int64 `json:"entry_count"`
	FieldCount     int64 `json:"field_count"`
	MaxFieldBytes  int64 `json:"max_field_bytes"`
	CanaryPresent  bool  `json:"canary_present"`
	UnexpectedName bool  `json:"unexpected_name"`
}

type indexPendingEntry struct {
	ID         string
	Consumer   string
	IdleMillis int64
	Deliveries int64
}

type indexSSEObservation struct {
	EventCount    int
	EventTypes    []string
	CanaryPresent bool
}

type indexComposeHarness struct {
	config indexReliabilityConfig
}

// TestExistingComposeIndexReliability is an explicit, state-changing local
// system test. The normal suite only compiles it. It never bootstraps
// credentials or modifies Centry source: all authentication and runtime
// material must already exist outside the repository.
func TestExistingComposeIndexReliability(t *testing.T) {
	if os.Getenv(indexReliabilityOptIn) != "1" {
		t.Skip("set ELITEA_INDEX_RELIABILITY_SYSTEM_TEST=1 to run against the existing local Centry runtime profile")
	}

	config := loadIndexReliabilityConfig(t)
	harness := &indexComposeHarness{config: config}
	ctx, cancel := context.WithTimeout(context.Background(), config.timeout)
	defer cancel()

	harness.requireCleanBaseline(t, ctx)

	type admittedExecution struct {
		id        string
		indexName string
	}
	var admittedMu sync.Mutex
	var admitted []admittedExecution
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		admittedMu.Lock()
		defer admittedMu.Unlock()
		for _, execution := range admitted {
			_ = harness.stopIndex(cleanupCtx, execution.id, execution.indexName)
		}
		_, _ = harness.compose(cleanupCtx, "start", indexWorkerService)
	})

	nonce := randomIndexReliabilityNonce(t)
	reclaimIndexName := "rel-reclaim-" + nonce
	reclaimCanary := "reliability-private-canary-reclaim-" + nonce
	reclaimBody := prepareIndexReliabilityRequest(t, config.startBody, reclaimIndexName, reclaimCanary)

	harness.stopWorker(t, ctx)
	reclaimExecution := harness.startIndex(t, ctx, reclaimBody)
	admittedMu.Lock()
	admitted = append(admitted, admittedExecution{id: reclaimExecution, indexName: reclaimIndexName})
	admittedMu.Unlock()

	reclaimSSECancel, reclaimSSEDone := harness.observeSSE(t, ctx, reclaimExecution, reclaimCanary)
	harness.waitForJob(t, ctx, reclaimExecution, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "DISPATCHED" && snapshot.Published
	}, "published DISPATCHED execution")
	reference := harness.waitForRedisReference(t, ctx, reclaimCanary, 1)
	assertReferenceOnlyIndexRedis(t, reference)

	entryID := harness.syntheticRead(t, ctx)
	harness.ageSyntheticPending(t, ctx, entryID)
	pending := harness.pendingEntries(t, ctx)
	if len(pending) != 1 || pending[0].ID != entryID ||
		pending[0].Consumer != syntheticConsumer || pending[0].Deliveries < 2 {
		t.Fatalf("synthetic crash fixture did not leave one aged pending reference: %+v", pending)
	}

	harness.startWorker(t, ctx)
	reclaimTerminal := harness.waitForJob(t, ctx, reclaimExecution, func(snapshot indexJobSnapshot) bool {
		return isIndexTerminal(snapshot.State) && snapshot.Claims > 0
	}, "terminal execution claimed by the restarted real worker")
	if reclaimTerminal.State != "SUCCEEDED" && reclaimTerminal.State != "FAILED" {
		t.Fatalf("redelivered execution terminal state = %q, want SUCCEEDED or FAILED", reclaimTerminal.State)
	}
	if reclaimTerminal.ReplayEvents == 0 {
		t.Fatal("redelivered execution has no durable replay events")
	}
	harness.waitForRedisEmpty(t, ctx, reclaimCanary)
	reclaimSSE := finishIndexSSE(t, reclaimSSECancel, reclaimSSEDone)
	assertSafeIndexSSE(t, reclaimSSE)
	t.Logf(
		"real worker reclaimed the synthetic crash-window delivery: state=%s claims=%d max_claim_attempt=%d replay_events=%d",
		reclaimTerminal.State,
		reclaimTerminal.Claims,
		reclaimTerminal.MaxClaimAttempt,
		reclaimTerminal.ReplayEvents,
	)

	stopIndexName := "rel-stop-" + nonce
	stopCanary := "reliability-private-canary-stop-" + nonce
	stopBody := prepareIndexReliabilityRequest(t, config.startBody, stopIndexName, stopCanary)

	harness.stopWorker(t, ctx)
	stopExecution := harness.startIndex(t, ctx, stopBody)
	admittedMu.Lock()
	admitted = append(admitted, admittedExecution{id: stopExecution, indexName: stopIndexName})
	admittedMu.Unlock()

	stopSSECancel, stopSSEDone := harness.observeSSE(t, ctx, stopExecution, stopCanary)
	harness.waitForJob(t, ctx, stopExecution, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "DISPATCHED" && snapshot.Published
	}, "published stop target")
	assertReferenceOnlyIndexRedis(t, harness.waitForRedisReference(t, ctx, stopCanary, 1))

	if err := harness.stopIndex(ctx, stopExecution, stopIndexName); err != nil {
		t.Fatalf("public Stop request: %v", err)
	}
	// The current route is deliberately idempotent and keeps returning 204.
	if err := harness.stopIndex(ctx, stopExecution, stopIndexName); err != nil {
		t.Fatalf("idempotent public Stop request: %v", err)
	}
	stopTerminal := harness.waitForJob(t, ctx, stopExecution, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "CANCELLED" && snapshot.DesiredState == "CANCELLED"
	}, "durably cancelled execution")
	if stopTerminal.Claims != 0 || !stopTerminal.Retired || stopTerminal.ReplayEvents == 0 {
		t.Fatalf("no-authority Stop did not durably retire without a worker claim: %+v", stopTerminal)
	}
	// Redis retirement remains consumer-owned: the producer credential cannot
	// XACK/XDEL a command, and no business claim is needed for the worker to
	// receive the durable retired decision. Restarting the real worker must
	// drain this tiny reference without invoking the SDK or creating a claim.
	harness.startWorker(t, ctx)
	harness.waitForRedisEmpty(t, ctx, stopCanary)
	stopTerminal = harness.waitForJob(t, ctx, stopExecution, func(snapshot indexJobSnapshot) bool {
		return snapshot.State == "CANCELLED" &&
			snapshot.DesiredState == "CANCELLED" &&
			snapshot.Claims == 0 &&
			snapshot.Retired
	}, "cancelled execution drained without business authority")
	stopSSE := finishIndexSSE(t, stopSSECancel, stopSSEDone)
	assertSafeIndexSSE(t, stopSSE)
}

func loadIndexReliabilityConfig(t *testing.T) indexReliabilityConfig {
	t.Helper()
	centryDir := requiredAbsoluteDirectory(t, "ELITEA_CENTRY_DIR")
	runtimeDir := requiredAbsoluteDirectory(t, "ELITEA_AUTH_POV_RUNTIME_DIR")
	cookiePath := requiredAbsoluteFile(t, "ELITEA_INDEX_TEST_COOKIE_FILE", true)
	requestPath := requiredAbsoluteFile(t, "ELITEA_INDEX_TEST_REQUEST_FILE", false)

	if _, err := os.Stat(filepath.Join(centryDir, "docker-compose.yml")); err != nil {
		t.Fatalf("ELITEA_CENTRY_DIR does not contain docker-compose.yml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(centryDir, "hybrid_auth", "docker-compose.pov.yml")); err != nil {
		t.Fatalf("ELITEA_CENTRY_DIR does not contain the hybrid runtime overlay: %v", err)
	}

	cookieBytes, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("read cookie file: %v", err)
	}
	cookie := strings.TrimSpace(string(cookieBytes))
	if len(cookie) == 0 || len(cookie) > 4096 || strings.ContainsAny(cookie, "\r\n") ||
		!strings.Contains(cookie, "=") || strings.HasPrefix(strings.ToLower(cookie), "cookie:") {
		t.Fatal("ELITEA_INDEX_TEST_COOKIE_FILE must contain only a bounded raw Cookie header value")
	}

	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read request file: %v", err)
	}
	startBody, toolkitID, err := decodeNonSecretIndexStartBody(requestBytes)
	if err != nil {
		t.Fatalf("invalid ELITEA_INDEX_TEST_REQUEST_FILE: %v", err)
	}

	projectID, err := strconv.ParseInt(os.Getenv("ELITEA_INDEX_TEST_PROJECT_ID"), 10, 64)
	if err != nil || projectID <= 0 {
		t.Fatal("ELITEA_INDEX_TEST_PROJECT_ID must be a positive integer")
	}

	baseURL := strings.TrimSuffix(os.Getenv("ELITEA_INDEX_TEST_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "https://localhost:18443"
	}
	parsedBase, err := url.Parse(baseURL)
	if err != nil || parsedBase.Scheme != "https" || parsedBase.Host == "" ||
		parsedBase.User != nil || parsedBase.RawQuery != "" || parsedBase.Fragment != "" ||
		(parsedBase.Path != "" && parsedBase.Path != "/") {
		t.Fatal("ELITEA_INDEX_TEST_BASE_URL must be an HTTPS origin")
	}

	caBytes, err := os.ReadFile(filepath.Join(runtimeDir, "runtime", "runtime-ca.crt"))
	if err != nil {
		t.Fatalf("read runtime public CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caBytes) {
		t.Fatal("runtime public CA file contains no certificate")
	}

	timeout := 5 * time.Minute
	if raw := os.Getenv("ELITEA_INDEX_TEST_TIMEOUT"); raw != "" {
		timeout, err = time.ParseDuration(raw)
		if err != nil || timeout < time.Minute || timeout > 15*time.Minute {
			t.Fatal("ELITEA_INDEX_TEST_TIMEOUT must be between 1m and 15m")
		}
	}

	composeEnv := append([]string(nil), os.Environ()...)
	composeEnv = append(composeEnv,
		"ELITEA_AUTH_POV_RUNTIME_DIR="+runtimeDir,
		"ELITEA_RUNTIME_ENABLED=true",
		"ELITEA_CONFIGURATIONS_MUTATION_ENABLED=true",
		"ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED=true",
		"ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM="+indexCommandStream,
		"ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP="+indexConsumer,
		"ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES=64",
		"ELITEA_INDEX_ROUTE_FILE=./hybrid_auth/traefik-index-routes.yml",
	)

	return indexReliabilityConfig{
		centryDir:      centryDir,
		runtimeDir:     runtimeDir,
		composeProject: strings.TrimSpace(os.Getenv("ELITEA_INDEX_5681_COMPOSE_PROJECT")),
		baseURL:        baseURL,
		projectID:      projectID,
		cookie:         cookie,
		startBody:      startBody,
		toolkitID:      toolkitID,
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
					RootCAs:    roots,
				},
			},
			Timeout: 15 * time.Second,
		},
		composeEnv: composeEnv,
		timeout:    timeout,
	}
}

func requiredAbsoluteDirectory(t *testing.T, environmentName string) string {
	t.Helper()
	value := os.Getenv(environmentName)
	if !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", environmentName)
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		t.Fatalf("resolve %s: %v", environmentName, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		t.Fatalf("%s must name an existing directory", environmentName)
	}
	return resolved
}

func requiredAbsoluteFile(t *testing.T, environmentName string, private bool) string {
	t.Helper()
	value := os.Getenv(environmentName)
	if !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", environmentName)
	}
	info, err := os.Lstat(value)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s must name a regular non-symlink file", environmentName)
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s must not grant group or other permissions", environmentName)
	}
	return value
}

func decodeNonSecretIndexStartBody(raw []byte) (map[string]any, int64, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, 0, errors.New("index start request must be between 1 byte and 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, 0, fmt.Errorf("decode index start request: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, 0, errors.New("index start request must contain exactly one JSON object")
	}
	if containsCredentialShapedKey(body) {
		return nil, 0, errors.New("index start request contains credential-shaped material; use a server-side Configuration reference")
	}
	if body["tool_name"] != "index_data" {
		return nil, 0, errors.New("index start request tool_name must be index_data")
	}
	toolkit, ok := body["toolkit_config"].(map[string]any)
	if !ok || len(toolkit) != 1 {
		return nil, 0, errors.New("toolkit_config must contain only toolkit_id")
	}
	rawToolkitID, ok := toolkit["toolkit_id"].(json.Number)
	if !ok {
		return nil, 0, errors.New("toolkit_config.toolkit_id must be a positive integer")
	}
	toolkitID, err := strconv.ParseInt(string(rawToolkitID), 10, 64)
	if err != nil || toolkitID <= 0 {
		return nil, 0, errors.New("toolkit_config.toolkit_id must be a positive integer")
	}
	params, ok := body["tool_params"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("tool_params must be an object")
	}
	if _, ok := params["index_name"].(string); !ok {
		return nil, 0, errors.New("tool_params.index_name must be a string; the harness replaces it with a unique name")
	}
	return body, toolkitID, nil
}

func containsCredentialShapedKey(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			for _, forbidden := range []string{
				"password", "secret", "token", "credential", "authorization",
				"cookie", "api_key", "private_key",
			} {
				if strings.Contains(normalized, forbidden) {
					return true
				}
			}
			if containsCredentialShapedKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsCredentialShapedKey(child) {
				return true
			}
		}
	}
	return false
}

func prepareIndexReliabilityRequest(
	t *testing.T,
	template map[string]any,
	indexName string,
	canary string,
) []byte {
	t.Helper()
	clonedBytes, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(clonedBytes, &cloned); err != nil {
		t.Fatal(err)
	}
	params, ok := cloned["tool_params"].(map[string]any)
	if !ok {
		t.Fatal("tool_params disappeared while cloning start request")
	}
	params["index_name"] = indexName
	// This non-secret marker is deliberately persisted on the private content
	// plane. It must not appear in the Redis reference or public SSE payload.
	params["__elitea_reliability_canary"] = canary
	cloned["stream_id"] = "index-reliability-" + indexName
	cloned["message_id"] = "index-reliability-" + indexName
	result, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func randomIndexReliabilityNonce(t *testing.T) string {
	t.Helper()
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(value)
}

func (h *indexComposeHarness) requireCleanBaseline(t *testing.T, ctx context.Context) {
	t.Helper()
	output, err := h.compose(ctx, "ps", "--status", "running", "--services")
	if err != nil {
		t.Fatalf("list running compose services: %v", err)
	}
	running := make(map[string]bool)
	for _, service := range strings.Fields(output) {
		running[service] = true
	}
	for _, service := range []string{"postgres", "runtime_redis", "elitea-main", "auth_gateway", indexWorkerService} {
		if !running[service] {
			t.Fatalf("required compose service %q is not running", service)
		}
	}

	active := h.postgresScalar(t, ctx, `
SELECT count(*)
FROM elitea_runtime.execution_jobs
WHERE capability_id = 'index.ingest.v1'
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`)
	if active != "0" {
		t.Fatalf("shared compose has %s active index executions; refusing to disturb it", active)
	}
	reference := h.redisReference(t, ctx, "")
	pending := h.pendingEntries(t, ctx)
	if reference.Length != 0 || reference.Mappings != 0 || len(pending) != 0 {
		t.Fatalf("shared compose has existing index control state; refusing to disturb it: redis=%+v pending=%+v", reference, pending)
	}
}

func (h *indexComposeHarness) startIndex(t *testing.T, ctx context.Context, body []byte) string {
	t.Helper()
	endpoint := fmt.Sprintf(
		"%s/api/v2/elitea_core/test_toolkit_tool/prompt_lib/%d?await_response=false&execution_contract=index.ingest.v1",
		h.config.baseURL,
		h.config.projectID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", h.config.cookie)
	response, err := h.config.httpClient.Do(request)
	if err != nil {
		t.Fatalf("public index start: %v", err)
	}
	defer closeIndexReliabilityResource(t, response.Body, "public index start response")
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read public index start response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public index start status=%d", response.StatusCode)
	}
	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || !executionIDPattern.MatchString(result.TaskID) {
		t.Fatalf("public index start returned invalid task identity")
	}
	return result.TaskID
}

func (h *indexComposeHarness) stopIndex(
	ctx context.Context,
	executionID,
	indexName string,
) (resultErr error) {
	if !executionIDPattern.MatchString(executionID) {
		return errors.New("invalid execution identity")
	}
	endpoint := fmt.Sprintf(
		"%s/api/v2/elitea_core/index_cancel/prompt_lib/%d/%d/%s/%s",
		h.config.baseURL,
		h.config.projectID,
		h.config.toolkitID,
		url.PathEscape(indexName),
		executionID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Cookie", h.config.cookie)
	response, err := h.config.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer joinIndexReliabilityCloseError(
		&resultErr,
		response.Body,
		"public Stop response",
	)
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status=%d", response.StatusCode)
	}
	return nil
}

func (h *indexComposeHarness) observeSSE(
	t *testing.T,
	parent context.Context,
	executionID string,
	canary string,
) (context.CancelFunc, <-chan indexSSEObservation) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan indexSSEObservation, 1)
	ready := make(chan error, 1)
	go func() {
		observation, err := h.collectSSE(ctx, executionID, canary, ready)
		if err != nil && !errors.Is(err, context.Canceled) {
			ready <- err
		}
		done <- observation
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			t.Fatalf("open public SSE: %v", err)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("public SSE did not return response headers")
	}
	return cancel, done
}

func (h *indexComposeHarness) collectSSE(
	ctx context.Context,
	executionID string,
	canary string,
	ready chan<- error,
) (observation indexSSEObservation, resultErr error) {
	endpoint := fmt.Sprintf(
		"%s/api/v2/executions/%d/%s/events",
		h.config.baseURL,
		h.config.projectID,
		executionID,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return indexSSEObservation{}, err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cookie", h.config.cookie)
	client := *h.config.httpClient
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		return indexSSEObservation{}, err
	}
	defer joinIndexReliabilityCloseError(
		&resultErr,
		response.Body,
		"public SSE response",
	)
	if response.StatusCode != http.StatusOK ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return indexSSEObservation{}, fmt.Errorf(
			"status=%d content-type=%q",
			response.StatusCode,
			response.Header.Get("Content-Type"),
		)
	}
	ready <- nil

	var eventType string
	var data strings.Builder
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 128*1024)
	flush := func() {
		if eventType == "" {
			data.Reset()
			return
		}
		observation.EventCount++
		observation.EventTypes = append(observation.EventTypes, eventType)
		if strings.Contains(data.String(), canary) {
			observation.CanaryPresent = true
		}
		eventType = ""
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			flush()
		}
	}
	flush()
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return observation, err
	}
	return observation, ctx.Err()
}

func finishIndexSSE(
	t *testing.T,
	cancel context.CancelFunc,
	done <-chan indexSSEObservation,
) indexSSEObservation {
	t.Helper()
	// The production replay waiter polls every two seconds. Give it one full
	// poll after the durable terminal snapshot before closing the client.
	time.Sleep(2500 * time.Millisecond)
	cancel()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("public SSE did not close after client cancellation")
		return indexSSEObservation{}
	}
}

func assertSafeIndexSSE(t *testing.T, observation indexSSEObservation) {
	t.Helper()
	if observation.EventCount == 0 {
		t.Fatal("public SSE returned no durable execution event")
	}
	if observation.CanaryPresent {
		t.Fatalf("public SSE leaked the private content-plane canary; event types=%v", observation.EventTypes)
	}
}

func (h *indexComposeHarness) waitForJob(
	t *testing.T,
	ctx context.Context,
	executionID string,
	accept func(indexJobSnapshot) bool,
	description string,
) indexJobSnapshot {
	t.Helper()
	var last indexJobSnapshot
	err := pollIndexReliability(ctx, 200*time.Millisecond, func() (bool, error) {
		snapshot, err := h.jobSnapshot(ctx, executionID)
		if err != nil {
			return false, err
		}
		last = snapshot
		return accept(snapshot), nil
	})
	if err != nil {
		t.Fatalf("wait for %s: %v (last=%+v)", description, err, last)
	}
	return last
}

func (h *indexComposeHarness) jobSnapshot(ctx context.Context, executionID string) (indexJobSnapshot, error) {
	if !executionIDPattern.MatchString(executionID) {
		return indexJobSnapshot{}, errors.New("invalid execution identity")
	}
	query := fmt.Sprintf(`
SELECT json_build_object(
    'state', j.state,
    'desired_state', j.desired_state,
    'terminal_error', COALESCE(j.terminal_error_code, ''),
    'claims', (SELECT count(*) FROM elitea_runtime.execution_claims AS c
               WHERE c.execution_id = j.execution_id AND c.generation = j.generation),
    'max_claim_attempt', COALESCE((SELECT max(c.claim_attempt)
                                  FROM elitea_runtime.execution_claims AS c
                                  WHERE c.execution_id = j.execution_id
                                    AND c.generation = j.generation), 0),
    'replay_events', (SELECT count(*) FROM elitea_runtime.execution_replay_events AS r
                      WHERE r.execution_id = j.execution_id AND r.generation = j.generation),
    'published', o.published_at IS NOT NULL,
    'authority_granted', o.authority_granted_at IS NOT NULL,
    'retired', o.retired_at IS NOT NULL
)
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = '%s' AND j.capability_id = 'index.ingest.v1'`, executionID)
	output, err := h.postgres(ctx, query)
	if err != nil {
		return indexJobSnapshot{}, err
	}
	if strings.TrimSpace(output) == "" {
		return indexJobSnapshot{}, errors.New("execution is not durably visible")
	}
	var snapshot indexJobSnapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &snapshot); err != nil {
		return indexJobSnapshot{}, fmt.Errorf("decode PostgreSQL execution snapshot: %w", err)
	}
	return snapshot, nil
}

func isIndexTerminal(state string) bool {
	return state == "SUCCEEDED" || state == "FAILED" || state == "CANCELLED"
}

func (h *indexComposeHarness) waitForRedisReference(
	t *testing.T,
	ctx context.Context,
	canary string,
	length int64,
) indexRedisReferenceSnapshot {
	t.Helper()
	var last indexRedisReferenceSnapshot
	err := pollIndexReliability(ctx, 100*time.Millisecond, func() (bool, error) {
		snapshot, err := h.redisReferenceResult(ctx, canary)
		if err != nil {
			return false, err
		}
		last = snapshot
		return snapshot.Length == length && snapshot.Mappings == length, nil
	})
	if err != nil {
		t.Fatalf("wait for Redis reference: %v (last=%+v)", err, last)
	}
	return last
}

func (h *indexComposeHarness) waitForRedisEmpty(t *testing.T, ctx context.Context, canary string) {
	t.Helper()
	var reference indexRedisReferenceSnapshot
	var pending []indexPendingEntry
	err := pollIndexReliability(ctx, 200*time.Millisecond, func() (bool, error) {
		var err error
		reference, err = h.redisReferenceResult(ctx, canary)
		if err != nil {
			return false, err
		}
		pending, err = h.pendingEntriesResult(ctx)
		if err != nil {
			return false, err
		}
		return reference.Length == 0 && reference.Mappings == 0 && len(pending) == 0, nil
	})
	if err != nil {
		t.Fatalf("wait for Redis reference retirement: %v (redis=%+v pending=%+v)", err, reference, pending)
	}
}

func assertReferenceOnlyIndexRedis(t *testing.T, snapshot indexRedisReferenceSnapshot) {
	t.Helper()
	if snapshot.Length != 1 || snapshot.Mappings != 1 || snapshot.EntryCount != 1 ||
		snapshot.FieldCount != 1 || snapshot.MaxFieldBytes < 1 ||
		snapshot.MaxFieldBytes > 48*1024 || snapshot.UnexpectedName ||
		snapshot.CanaryPresent {
		t.Fatalf("Redis command is not one bounded reference-only signed envelope: %+v", snapshot)
	}
}

func (h *indexComposeHarness) redisReference(t *testing.T, ctx context.Context, canary string) indexRedisReferenceSnapshot {
	t.Helper()
	result, err := h.redisReferenceResult(ctx, canary)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (h *indexComposeHarness) redisReferenceResult(
	ctx context.Context,
	canary string,
) (indexRedisReferenceSnapshot, error) {
	script := `
local rows = redis.call('XRANGE', KEYS[1], '-', '+', 'COUNT', 2)
local fields = 0
local max_bytes = 0
local canary_present = false
local unexpected_name = false
for _, row in ipairs(rows) do
  for i = 1, #row[2], 2 do
    fields = fields + 1
    local name = row[2][i]
    local value = row[2][i + 1]
    if name ~= 'signed_envelope' then unexpected_name = true end
    if string.len(value) > max_bytes then max_bytes = string.len(value) end
    if ARGV[1] ~= '' and string.find(value, ARGV[1], 1, true) then canary_present = true end
  end
end
return cjson.encode({
  length = redis.call('XLEN', KEYS[1]),
  mappings = redis.call('HLEN', KEYS[2]),
  entry_count = #rows,
  field_count = fields,
  max_field_bytes = max_bytes,
  canary_present = canary_present,
  unexpected_name = unexpected_name
})`
	output, err := h.redis(ctx, "producer", "--raw", "EVAL", script, "2",
		indexCommandStream, indexCommandStream+":delivery-index.v1", canary)
	if err != nil {
		return indexRedisReferenceSnapshot{}, err
	}
	var snapshot indexRedisReferenceSnapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &snapshot); err != nil {
		return indexRedisReferenceSnapshot{}, fmt.Errorf("decode Redis reference snapshot: %w", err)
	}
	return snapshot, nil
}

func (h *indexComposeHarness) syntheticRead(t *testing.T, ctx context.Context) string {
	t.Helper()
	script := `
local rows = redis.call(
  'XREADGROUP', 'GROUP', ARGV[1], ARGV[2],
  'COUNT', 1, 'STREAMS', KEYS[1], '>'
)
if not rows then return '' end
return rows[1][2][1][1]`
	output, err := h.redis(ctx, "indexer-worker", "--raw", "EVAL", script, "1",
		indexCommandStream, indexConsumer, syntheticConsumer)
	if err != nil {
		t.Fatalf("inject synthetic crashed consumer: %v", err)
	}
	entryID := strings.TrimSpace(output)
	if !redisEntryPattern.MatchString(entryID) {
		t.Fatalf("synthetic crashed consumer returned invalid Redis entry identity")
	}
	return entryID
}

func (h *indexComposeHarness) ageSyntheticPending(t *testing.T, ctx context.Context, entryID string) {
	t.Helper()
	// Do not use XCLAIM JUSTID here: Redis deliberately does not increment the
	// delivery counter for JUSTID. The Lua wrapper performs the real claim but
	// returns only the entry ID, never the signed envelope bytes.
	script := `
local rows = redis.call(
  'XCLAIM', KEYS[1], ARGV[1], ARGV[2], 0, ARGV[3], 'IDLE', 61000
)
if #rows ~= 1 then return '' end
return rows[1][1]`
	output, err := h.redis(ctx, "indexer-worker", "--raw",
		"EVAL", script, "1", indexCommandStream,
		indexConsumer, syntheticConsumer, entryID)
	if err != nil {
		t.Fatalf("age synthetic pending reference: %v", err)
	}
	if strings.TrimSpace(output) != entryID {
		t.Fatalf("age synthetic pending reference returned unexpected identity")
	}
}

func (h *indexComposeHarness) pendingEntries(t *testing.T, ctx context.Context) []indexPendingEntry {
	t.Helper()
	entries, err := h.pendingEntriesResult(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func (h *indexComposeHarness) pendingEntriesResult(ctx context.Context) ([]indexPendingEntry, error) {
	output, err := h.redis(ctx, "indexer-worker", "--json",
		"XPENDING", indexCommandStream, indexConsumer, "-", "+", "10")
	if err != nil {
		return nil, err
	}
	var raw [][]any
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(output)))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode Redis pending entries: %w", err)
	}
	entries := make([]indexPendingEntry, 0, len(raw))
	for _, row := range raw {
		if len(row) != 4 {
			return nil, errors.New("Redis pending entry has unexpected shape")
		}
		id, idOK := row[0].(string)
		consumer, consumerOK := row[1].(string)
		idle, idleOK := jsonNumberInt64(row[2])
		deliveries, deliveriesOK := jsonNumberInt64(row[3])
		if !idOK || !consumerOK || !idleOK || !deliveriesOK ||
			!redisEntryPattern.MatchString(id) || consumer == "" ||
			idle < 0 || deliveries < 1 {
			return nil, errors.New("Redis pending entry contains invalid values")
		}
		entries = append(entries, indexPendingEntry{
			ID: id, Consumer: consumer, IdleMillis: idle, Deliveries: deliveries,
		})
	}
	return entries, nil
}

func jsonNumberInt64(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	result, err := strconv.ParseInt(string(number), 10, 64)
	return result, err == nil
}

func (h *indexComposeHarness) stopWorker(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := h.compose(ctx, "stop", "--timeout", "10", indexWorkerService); err != nil {
		t.Fatalf("stop real indexer worker: %v", err)
	}
}

func (h *indexComposeHarness) startWorker(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := h.compose(ctx, "start", indexWorkerService); err != nil {
		t.Fatalf("start real indexer worker: %v", err)
	}
}

func pollIndexReliability(
	ctx context.Context,
	interval time.Duration,
	condition func() (bool, error),
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		matched, err := condition()
		if err == nil && matched {
			return nil
		}
		// Compose process startup and database visibility can race briefly.
		// The last bounded failure is retained and returned if the caller times out.
		select {
		case <-ctx.Done():
			if err != nil {
				return errors.Join(ctx.Err(), err)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func closeIndexReliabilityResource(
	t *testing.T,
	resource io.Closer,
	description string,
) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Errorf("close %s: %v", description, err)
	}
}

func joinIndexReliabilityCloseError(
	resultErr *error,
	resource io.Closer,
	description string,
) {
	if err := resource.Close(); err != nil {
		*resultErr = errors.Join(
			*resultErr,
			fmt.Errorf("close %s: %w", description, err),
		)
	}
}

func (h *indexComposeHarness) postgresScalar(t *testing.T, ctx context.Context, query string) string {
	t.Helper()
	output, err := h.postgres(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(output)
}

func (h *indexComposeHarness) postgres(ctx context.Context, query string) (string, error) {
	return h.compose(
		ctx,
		"exec", "-T", "postgres",
		"sh", "-ec",
		`export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"`,
		"sh",
		query,
	)
}

func (h *indexComposeHarness) redis(
	ctx context.Context,
	role string,
	arguments ...string,
) (string, error) {
	var passwordPattern string
	switch role {
	case "producer":
		passwordPattern = `s/^user producer on >\([^ ]*\).*/\1/p`
	case "indexer-worker":
		passwordPattern = `s/^user indexer-worker on >\([^ ]*\).*/\1/p`
	default:
		return "", errors.New("unsupported Redis harness role")
	}
	script := `
password="$(sed -n "$1" /run/elitea-runtime/redis-users.acl)"
test -n "$password"
export REDISCLI_AUTH="$password"
role="$2"
shift 2
exec redis-cli --user "$role" --tls \
  --cacert /run/elitea-runtime/runtime-ca.crt \
  -h 127.0.0.1 -p 6380 "$@"`
	commandArguments := []string{
		"exec", "-T", "runtime_redis", "sh", "-ec", script, "sh",
		passwordPattern, role,
	}
	commandArguments = append(commandArguments, arguments...)
	return h.compose(ctx, commandArguments...)
}

func (h *indexComposeHarness) compose(ctx context.Context, arguments ...string) (string, error) {
	base := []string{"compose"}
	if h.config.composeProject != "" {
		base = append(base, "--project-name", h.config.composeProject)
	}
	base = append(base,
		"--project-directory", h.config.centryDir,
		"--env-file", filepath.Join(h.config.centryDir, "envs", "default.env"),
		"--env-file", filepath.Join(h.config.centryDir, "envs", "override.env"),
		"-f", filepath.Join(h.config.centryDir, "docker-compose.yml"),
		"-f", filepath.Join(h.config.centryDir, "hybrid_auth", "docker-compose.pov.yml"),
		"--profile", "runtime",
	)
	command := exec.CommandContext(ctx, "docker", append(base, arguments...)...)
	command.Dir = h.config.centryDir
	command.Env = h.config.composeEnv
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf(
			"docker compose %s: %w",
			strings.Join(arguments[:min(len(arguments), 3)], " "),
			err,
		)
	}
	return stdout.String(), nil
}

func TestDecodeNonSecretIndexStartBodyRejectsCredentialMaterial(t *testing.T) {
	for _, body := range []string{
		`{"toolkit_config":{"toolkit_id":7,"settings":{}},"tool_name":"index_data","tool_params":{"index_name":"docs"}}`,
		`{"toolkit_config":{"toolkit_id":7},"tool_name":"index_data","tool_params":{"index_name":"docs","token":"value"}}`,
		`{"toolkit_config":{"toolkit_id":7},"tool_name":"index_data","tool_params":{"index_name":"docs","nested":{"api-key":"value"}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, _, err := decodeNonSecretIndexStartBody([]byte(body)); err == nil {
				t.Fatal("credential-shaped request was accepted")
			}
		})
	}
}

func TestPrepareIndexReliabilityRequestInjectsOnlyNonSecretCanary(t *testing.T) {
	template := map[string]any{
		"toolkit_config": map[string]any{"toolkit_id": json.Number("7")},
		"tool_name":      "index_data",
		"tool_params":    map[string]any{"index_name": "docs", "recursive": true},
	}
	result := prepareIndexReliabilityRequest(t, template, "rel-reclaim-1234", "private-canary-1234")
	if bytes.Contains(result, []byte(`"index_name":"docs"`)) ||
		!bytes.Contains(result, []byte(`"index_name":"rel-reclaim-1234"`)) ||
		!bytes.Contains(result, []byte(`"__elitea_reliability_canary":"private-canary-1234"`)) {
		t.Fatalf("prepared request = %s", result)
	}
	originalParams := template["tool_params"].(map[string]any)
	if originalParams["index_name"] != "docs" || originalParams["__elitea_reliability_canary"] != nil {
		t.Fatal("request preparation mutated the caller template")
	}
}
