package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

const interruptContractDir = "../../../../../../libs/jsonschema/runtime/v1"

var interruptSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	paths, err := filepath.Glob(filepath.Join(interruptContractDir, "fanout-*.schema.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		stem := strings.TrimSuffix(filepath.Base(path), ".schema.json")
		if err := compiler.AddResource("https://schemas.elitea.ai/runtime/v1/elitea.pipeline."+stem+".v1", document); err != nil {
			return nil, err
		}
	}
	out := map[string]*jsonschema.Schema{}
	for _, stem := range []string{"fanout-interrupt-list", "fanout-interrupt-decision-result", "fanout-interrupt-error"} {
		compiled, err := compiler.Compile("https://schemas.elitea.ai/runtime/v1/elitea.pipeline." + stem + ".v1")
		if err != nil {
			return nil, err
		}
		out[stem] = compiled
	}
	return out, nil
})

// assertContractBody validates a response body against the D2 schema.
func assertContractBody(t *testing.T, stem string, body []byte) {
	t.Helper()
	schemas, err := interruptSchemas()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v: %s", stem, err, body)
	}
	if err := schemas[stem].Validate(instance); err != nil {
		t.Fatalf("%s: body does not match the contract: %v\n%s", stem, err, body)
	}
}

func interruptFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(interruptContractDir, "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type interruptUseCaseStub struct {
	mu        sync.Mutex
	calls     int
	input     domain.DecideInput
	selector  domain.Selector
	result    domain.DecideResult
	list      domain.List
	err       error
	listCalls int
}

func (s *interruptUseCaseStub) List(_ context.Context, selector domain.Selector) (domain.List, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.selector = selector
	return s.list, s.err
}

func (s *interruptUseCaseStub) Decide(_ context.Context, input domain.DecideInput) (domain.DecideResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.input = input
	return s.result, s.err
}

const (
	interruptTestResponse = "10000000-0000-4000-8000-000000000043"
	interruptTestKey      = "93ba421dbbbcc4f07e5b3366f081d1ef3ac983c422a3b9e48ed1fbe05b2d6a87"
)

func newInterruptTestRoute(t *testing.T, useCase ExecutionInterruptUseCase, granted ...string) http.Handler {
	t.Helper()
	config := apimw.AuthConfig{
		PrincipalValidator: currentStartPrincipalValidatorFunc(func(_ context.Context, u auth.User) (auth.User, error) { return u, nil }),
		ForwardedIdentityVerifier: currentStartPeerVerifierFunc(func(r *http.Request) error {
			if r.RemoteAddr != "10.0.0.8:43120" {
				return errors.New("peer denied")
			}
			return nil
		}),
	}
	permissions := currentStartPermissionResolverFunc(func(_ context.Context, _ auth.User, mode, project string) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault || project != "7" {
			return auth.PermissionResolution{UserID: 11, Permissions: []string{}}, nil
		}
		return auth.PermissionResolution{UserID: 11, Permissions: append([]string{}, granted...)}, nil
	})
	route, err := NewCurrentExecutionInterruptRoute(useCase, config, permissions)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func interruptRequest(method, path string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Auth-Type", "user")
	req.Header.Set("X-Auth-ID", "11")
	req.RemoteAddr = "10.0.0.8:43120"
	return req
}

func decisionPath(project, response, key string) string {
	return "/api/v2/elitea_core/task/prompt_lib/" + project + "/" + response + "/interrupts/" + key + "/decision"
}

func TestExecutionInterruptDecisionStrictAdmission(t *testing.T) {
	approve := interruptFixture(t, "fanout-interrupt-decision-request-v1.json")
	oversized := []byte(`{"action":"edit","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":"` + strings.Repeat("v", domain.MaxDecisionBodyBytes) + `"}`)
	ok := domain.DecideResult{InterruptKey: interruptTestKey, State: domain.StateDecided, Revision: 2, RequestID: "0b7672bbbf4eb882481748fb8a0ce16c20f873b838e311a64e6442376c7a5a25"}
	for _, spec := range []struct {
		name     string
		path     string
		body     []byte
		granted  []string
		result   domain.DecideResult
		err      error
		status   int
		calls    int
		envelope string
	}{
		{"applied", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, ok, nil, 200, 1, ""},
		{"replay", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, func() domain.DecideResult { r := ok; r.Replay = true; return r }(), nil, 200, 1, ""},
		{"already resolved", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, domain.DecideResult{}, domain.ErrAlreadyResolved, 409, 1, "agent_interrupt_already_resolved"},
		{"no card", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, domain.DecideResult{}, domain.ErrNotFound, 404, 1, "agent_interrupt_not_found"},
		{"action not offered", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, domain.DecideResult{}, domain.ErrInvalidDecision, 400, 1, "agent_interrupt_invalid_decision"},
		{"not the owner", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, domain.DecideResult{}, domain.ErrNotAllowed, 403, 1, ""},
		{"no permission", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionList}, ok, nil, 403, 0, ""},
		{"foreign project", decisionPath("8", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, ok, nil, 403, 0, ""},
		{"uppercase key", decisionPath("7", interruptTestResponse, strings.ToUpper(interruptTestKey)), approve, []string{domain.PermissionDecide}, ok, nil, 404, 0, "agent_interrupt_not_found"},
		{"zero key", decisionPath("7", interruptTestResponse, strings.Repeat("0", 64)), approve, []string{domain.PermissionDecide}, ok, nil, 404, 0, "agent_interrupt_not_found"},
		{"bad response id", decisionPath("7", "not-a-uuid", interruptTestKey), approve, []string{domain.PermissionDecide}, ok, nil, 404, 0, "agent_interrupt_not_found"},
		{"query string", decisionPath("7", interruptTestResponse, interruptTestKey) + "?value=x", approve, []string{domain.PermissionDecide}, ok, nil, 400, 0, "agent_interrupt_invalid_decision"},
		{"over 8192 bytes", decisionPath("7", interruptTestResponse, interruptTestKey), oversized, []string{domain.PermissionDecide}, ok, nil, 400, 0, "agent_interrupt_invalid_decision"},
		{"empty body", decisionPath("7", interruptTestResponse, interruptTestKey), nil, []string{domain.PermissionDecide}, ok, nil, 400, 0, "agent_interrupt_invalid_decision"},
		{"store failure", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, domain.DecideResult{}, errors.New("database down"), 500, 1, ""},
		{"deadline", decisionPath("7", interruptTestResponse, interruptTestKey), approve, []string{domain.PermissionDecide}, domain.DecideResult{}, context.DeadlineExceeded, 504, 1, ""},
	} {
		t.Run(spec.name, func(t *testing.T) {
			useCase := &interruptUseCaseStub{result: spec.result, err: spec.err}
			w := httptest.NewRecorder()
			newInterruptTestRoute(t, useCase, spec.granted...).ServeHTTP(w, interruptRequest(http.MethodPost, spec.path, spec.body))
			if w.Code != spec.status || useCase.calls != spec.calls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, useCase.calls, w.Body.String())
			}
			if spec.status == 200 {
				assertContractBody(t, "fanout-interrupt-decision-result", w.Body.Bytes())
				if !bytes.Equal(useCase.input.Canonical, approve) || useCase.input.InterruptKey != interruptTestKey ||
					useCase.input.Selector != (domain.Selector{ProjectID: 7, ActorUserID: 11, ResponseMessageID: interruptTestResponse}) {
					t.Fatalf("use case input = %+v", useCase.input)
				}
			}
			if spec.envelope != "" {
				assertContractBody(t, "fanout-interrupt-error", w.Body.Bytes())
				if !strings.Contains(w.Body.String(), `"error":"`+spec.envelope+`"`) {
					t.Fatalf("envelope = %s", w.Body.String())
				}
			}
			if spec.status >= 500 && strings.Contains(w.Body.String(), "database down") {
				t.Fatal("internal error text reached the client")
			}
		})
	}
}

// Every invalid decision-request fixture is refused at the boundary before
// the ledger is called; every valid one reaches it.
func TestExecutionInterruptDecisionUsesContractFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(interruptContractDir, "fixtures", "fanout-interrupt-decision-request-v1*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatal(paths, err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		useCase := &interruptUseCaseStub{result: domain.DecideResult{InterruptKey: interruptTestKey, State: domain.StateDecided, Revision: 2, RequestID: strings.Repeat("ab", 32)}}
		w := httptest.NewRecorder()
		newInterruptTestRoute(t, useCase, domain.PermissionDecide).ServeHTTP(w, interruptRequest(http.MethodPost, decisionPath("7", interruptTestResponse, interruptTestKey), raw))
		invalid := strings.Contains(filepath.Base(path), ".invalid.")
		if invalid && (w.Code != 400 || useCase.calls != 0) {
			t.Errorf("%s: status=%d calls=%d", filepath.Base(path), w.Code, useCase.calls)
		}
		if !invalid && (w.Code != 200 || useCase.calls != 1) {
			t.Errorf("%s: status=%d calls=%d body=%s", filepath.Base(path), w.Code, useCase.calls, w.Body.String())
		}
	}
}

func TestExecutionInterruptListMatchesContract(t *testing.T) {
	var fixture struct {
		DecisionRevision int64 `json:"decision_revision"`
		Interrupts       []struct {
			Card     json.RawMessage `json:"card"`
			State    domain.State    `json:"state"`
			Revision int64           `json:"revision"`
		} `json:"interrupts"`
	}
	if err := json.Unmarshal(interruptFixture(t, "fanout-interrupt-list-v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	list := domain.List{ResponseMessageID: interruptTestResponse, DecisionRevision: fixture.DecisionRevision}
	for _, item := range fixture.Interrupts {
		list.Interrupts = append(list.Interrupts, domain.ListedInterrupt{Card: item.Card, State: item.State, Revision: item.Revision})
	}
	path := "/api/v2/elitea_core/task/prompt_lib/7/" + interruptTestResponse + "/interrupts"
	for _, spec := range []struct {
		name    string
		list    domain.List
		err     error
		granted []string
		path    string
		status  int
	}{
		{"two open cards", list, nil, []string{domain.PermissionList}, path, 200},
		{"nothing raised", domain.List{ResponseMessageID: interruptTestResponse}, nil, []string{domain.PermissionList}, path, 200},
		{"no permission", list, nil, []string{domain.PermissionDecide}, path, 403},
		{"not the owner", domain.List{}, domain.ErrNotAllowed, []string{domain.PermissionList}, path, 403},
		{"query string", list, nil, []string{domain.PermissionList}, path + "?all=1", 400},
	} {
		t.Run(spec.name, func(t *testing.T) {
			useCase := &interruptUseCaseStub{list: spec.list, err: spec.err}
			w := httptest.NewRecorder()
			newInterruptTestRoute(t, useCase, spec.granted...).ServeHTTP(w, interruptRequest(http.MethodGet, spec.path, nil))
			if w.Code != spec.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if spec.status == 400 {
				assertContractBody(t, "fanout-interrupt-error", w.Body.Bytes())
			}
			if spec.status == 200 {
				assertContractBody(t, "fanout-interrupt-list", w.Body.Bytes())
				if useCase.selector != (domain.Selector{ProjectID: 7, ActorUserID: 11, ResponseMessageID: interruptTestResponse}) {
					t.Fatalf("selector = %+v", useCase.selector)
				}
			}
		})
	}
}

// TestInterruptLogsCarryNoValues captures every log line of a decision, its
// replay and its failure, and finds no decision value, display text, tool
// argument, credential reference or request id in them.
func TestInterruptLogsCarryNoValues(t *testing.T) {
	var captured bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const secretValue = "SECRET-EDIT-VALUE-do-not-log"
	const credentialRef = "tsr_SECRETREF0000000000"
	edit := []byte(`{"action":"edit","expected_revision":1,"request_id":"` + strings.Repeat("ab", 32) + `","value":"` + secretValue + `"}`)
	authorize := []byte(`{"action":"authorize","credential_ref":"` + credentialRef + `","expected_revision":1,"request_id":"` + strings.Repeat("cd", 32) + `","value":""}`)
	for _, spec := range []struct {
		body []byte
		err  error
	}{
		{edit, nil},
		{edit, errors.New("store failure")},
		{authorize, nil},
		{authorize, errors.New("store failure")},
	} {
		useCase := &interruptUseCaseStub{result: domain.DecideResult{InterruptKey: interruptTestKey, State: domain.StateDecided, Revision: 2, RequestID: strings.Repeat("ab", 32)}, err: spec.err}
		w := httptest.NewRecorder()
		newInterruptTestRoute(t, useCase, domain.PermissionDecide).ServeHTTP(w, interruptRequest(http.MethodPost, decisionPath("7", interruptTestResponse, interruptTestKey), spec.body))
	}
	logs := captured.String()
	if !strings.Contains(logs, "execution interrupt decided") || !strings.Contains(logs, "execution interrupt request failed") {
		t.Fatalf("expected both log lines, got:\n%s", logs)
	}
	for _, forbidden := range []string{secretValue, credentialRef, strings.Repeat("ab", 32), strings.Repeat("cd", 32), interruptTestKey, "Approve creating"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("log carries %q:\n%s", forbidden, logs)
		}
	}
}
