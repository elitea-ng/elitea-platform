package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	goBaseURL     string
	legacyBaseURL string
	authToken     string
	requireParity string
	projectID     string
	client        *http.Client
)

// TestMain sets up two independent harnesses that must not gate each other:
//
//  1. The legacy-parity fixtures below (TestApplicationsList, TestHealthz,
//     TestPredict, ...) diff the Go service against a running legacy
//     instance. They need CONTRACT_AUTH_TOKEN and a reachable legacy_url,
//     both only available in ci-contract.yml's weekly schedule/dispatch job
//     — requireLegacyParityCredentials skips each one individually when
//     authToken is unset, rather than this function exiting the whole
//     process before m.Run() the way it used to. That old early-exit gated
//     every test in this package shut by default, including any
//     unrelated, self-contained TestArtifact* test added later — S19 needs
//     those to run unconditionally (skipping per-test on their own,
//     unrelated RustFS/Postgres env vars — see artifact_harness_test.go).
//  2. setupArtifactSuite (artifact_harness_test.go) builds S19's
//     self-contained artifact-API harness, torn down after m.Run() returns
//     — TestMain, not t.Cleanup, is the right place for whole-binary
//     setup/teardown shared across many independent top-level test
//     functions.
func TestMain(m *testing.M) {
	goBaseURL = envOr("CONTRACT_GO_URL", "http://localhost:8080")
	legacyBaseURL = envOr("CONTRACT_LEGACY_URL", "http://localhost:8000")
	authToken = os.Getenv("CONTRACT_AUTH_TOKEN")
	requireParity = os.Getenv("CONTRACT_REQUIRE_PARITY")
	projectID = envOr("CONTRACT_PROJECT_ID", "test-project")
	client = &http.Client{Timeout: 10 * time.Second}

	teardownArtifactSuite := setupArtifactSuite()
	code := m.Run()
	teardownArtifactSuite()
	os.Exit(code)
}

// parityDecision is what an empty CONTRACT_AUTH_TOKEN means to the caller.
type parityDecision int

const (
	parityRun  parityDecision = iota // credentials present: diff the two stacks
	paritySkip                       // no credentials, and none were promised
	parityFail                       // no credentials, but the job said it would supply them
)

// decideParityGate separates "these fixtures are not applicable here" from
// "these fixtures were supposed to run and cannot".
//
// Both used to be a skip, and that is issue #309's Gate 1: ci-contract.yml's
// weekly job set CONTRACT_AUTH_TOKEN from a secret that is empty, every fixture
// printed SKIP, and the job concluded success. Four consecutive Monday runs
// reported a passing legacy-parity gate that had compared nothing. A gate whose
// only failure mode is silence is not a gate.
//
// So a job that intends to run the parity fixtures declares it by setting
// CONTRACT_REQUIRE_PARITY, and an absent token then FAILS instead of skipping.
// Every other caller — PRs, `task test`, a developer's laptop — leaves the
// variable unset and keeps the old, correct skip: those runs never promised
// credentials, so their silence is honest.
func decideParityGate(authToken, requireFlag string) parityDecision {
	if authToken != "" {
		return parityRun
	}
	switch strings.ToLower(strings.TrimSpace(requireFlag)) {
	case "", "0", "false", "no":
		return paritySkip
	default:
		return parityFail
	}
}

// requireLegacyParityCredentials skips the calling test when
// CONTRACT_AUTH_TOKEN is unset — the default everywhere except
// ci-contract.yml's dispatch job, which sets the real secret AND sets
// CONTRACT_REQUIRE_PARITY so that a missing secret is a failure there. See
// TestMain's doc comment for why this is a per-test skip rather than a
// whole-process early exit, and decideParityGate for why it is not always one.
func requireLegacyParityCredentials(t *testing.T) {
	t.Helper()
	switch decideParityGate(authToken, requireParity) {
	case parityRun:
		return
	case parityFail:
		t.Fatalf("CONTRACT_REQUIRE_PARITY is set but CONTRACT_AUTH_TOKEN is empty: " +
			"this job declared it would run the legacy-parity fixtures and cannot. " +
			"Supply the staging credentials or stop claiming this gate ran.")
	default:
		t.Skip("set CONTRACT_AUTH_TOKEN to run the legacy-parity contract tests")
	}
}

func TestApplicationsList(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/applications", projectID)
	compareEndpoints(t, "GET", path, nil)
}

func TestSkillsList(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/skills", projectID)
	compareEndpoints(t, "GET", path, nil)
}

func TestFoldersList(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/folders", projectID)
	compareEndpoints(t, "GET", path, nil)
}

func TestTagsList(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/tags", projectID)
	compareEndpoints(t, "GET", path, nil)
}

func TestConversationsList(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/conversations", projectID)
	compareEndpoints(t, "GET", path, nil)
}

func TestAnalytics(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/analytics", projectID)
	compareEndpoints(t, "GET", path, nil)
}

func TestHealthz(t *testing.T) {
	requireLegacyParityCredentials(t)
	goResp := doRequest(t, "GET", goBaseURL+"/healthz", nil)
	if goResp.StatusCode != http.StatusOK {
		t.Errorf("Go /healthz returned %d", goResp.StatusCode)
	}
}

type CompareResult struct {
	Endpoint     string
	GoStatus     int
	LegacyStatus int
	SchemaMatch  bool
	Diff         string
}

func compareEndpoints(t *testing.T, method, path string, body io.Reader) CompareResult {
	t.Helper()

	goResp := doRequest(t, method, goBaseURL+path, body)
	legacyResp := doRequest(t, method, legacyBaseURL+path, body)

	result := CompareResult{
		Endpoint:     fmt.Sprintf("%s %s", method, path),
		GoStatus:     goResp.StatusCode,
		LegacyStatus: legacyResp.StatusCode,
	}

	if goResp.StatusCode != legacyResp.StatusCode {
		t.Errorf("[%s] status mismatch: go=%d legacy=%d", result.Endpoint, goResp.StatusCode, legacyResp.StatusCode)
		return result
	}

	goBody, _ := io.ReadAll(goResp.Body)
	legacyBody, _ := io.ReadAll(legacyResp.Body)
	_ = goResp.Body.Close()
	_ = legacyResp.Body.Close()

	goKeys := extractTopLevelKeys(goBody)
	legacyKeys := extractTopLevelKeys(legacyBody)

	result.SchemaMatch = sameKeys(goKeys, legacyKeys)
	if !result.SchemaMatch {
		result.Diff = fmt.Sprintf("go keys: %v, legacy keys: %v", goKeys, legacyKeys)
		t.Errorf("[%s] schema mismatch: %s", result.Endpoint, result.Diff)
	}

	return result
}

func doRequest(t *testing.T, method, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request to %s failed: %v", url, err)
	}
	return resp
}

func extractTopLevelKeys(data []byte) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, k := range a {
		set[k] = true
	}
	for _, k := range b {
		if !set[k] {
			return false
		}
	}
	return true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" { // testpg:not-a-database: only the CONTRACT_* HTTP settings reach this helper
		return v
	}
	return fallback
}

func TestPredict(t *testing.T) {
	requireLegacyParityCredentials(t)
	path := fmt.Sprintf("/api/v2/projects/%s/predict", projectID)
	body := strings.NewReader(`{"prompt":"hello","agent_id":"test","stream":false}`)
	compareEndpoints(t, "POST", path, body)
}
