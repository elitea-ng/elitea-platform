package health

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const leakyCause = "failed to connect to `user=elitea database=elitea`: 10.0.0.5:5432 password=s3cret"

func fixed(state, cause string) func(context.Context) (string, string) {
	return func(context.Context) (string, string) { return state, cause }
}

func readinessOf(t *testing.T, checks []Check) (int, Status, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	RoutesWithDeps(Deps{Checks: checks}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	body := rec.Body.String()
	var status Status
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("readiness body %q: %v", body, err)
	}
	return rec.Code, status, body
}

func TestReadinessAnswersNotReadyForTheDatabaseAndDegradedForEverythingElse(t *testing.T) {
	db := func(state string) Check { return Check{Name: "db", Critical: true, Probe: fixed(state, leakyCause)} }
	nats := func(state string) Check { return Check{Name: "nats", Probe: fixed(state, leakyCause)} }
	runners := func(state string) Check { return Check{Name: "runners", Probe: fixed(state, leakyCause)} }
	cases := []struct {
		name       string
		checks     []Check
		wantCode   int
		wantStatus string
	}{
		{"all healthy", []Check{db(StateOK), nats(StateOK), runners(StateOK)}, http.StatusOK, "ready"},
		{"database down", []Check{db(StateUnavailable), nats(StateOK), runners(StateOK)}, http.StatusServiceUnavailable, "not_ready"},
		{"NATS reconnecting", []Check{db(StateOK), nats(StateReconnecting), runners(StateOK)}, http.StatusOK, "degraded"},
		{"a runner in backoff", []Check{db(StateOK), nats(StateOK), runners(StateDegraded)}, http.StatusOK, "degraded"},
		{"database down and NATS reconnecting", []Check{db(StateUnavailable), nats(StateReconnecting), runners(StateOK)}, http.StatusServiceUnavailable, "not_ready"},
		{"a probe that reports nothing counts as unavailable", []Check{db("")}, http.StatusServiceUnavailable, "not_ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, status, body := readinessOf(t, tc.checks)
			if code != tc.wantCode || status.Status != tc.wantStatus {
				t.Fatalf("readiness = %d %q, want %d %q", code, status.Status, tc.wantCode, tc.wantStatus)
			}
			for _, check := range tc.checks {
				if _, ok := status.Checks[check.Name]; !ok {
					t.Fatalf("body %s lacks check %q", body, check.Name)
				}
			}
			for _, leak := range []string{"5432", "s3cret", "user=", "10.0.0.5"} {
				if strings.Contains(body, leak) {
					t.Fatalf("readiness body %s leaks %q", body, leak)
				}
			}
		})
	}
}

func TestReadinessRunsItsChecksConcurrentlyWithinItsBound(t *testing.T) {
	slow := func(ctx context.Context) (string, string) {
		select {
		case <-time.After(300 * time.Millisecond):
			return StateOK, ""
		case <-ctx.Done():
			return StateUnavailable, "no answer in time"
		}
	}
	started := time.Now()
	code, _, _ := readinessOf(t, []Check{{Name: "a", Critical: true, Probe: slow}, {Name: "b", Critical: true, Probe: slow}, {Name: "c", Probe: slow}})
	if elapsed := time.Since(started); code != http.StatusOK || elapsed > 600*time.Millisecond {
		t.Fatalf("three 300 ms checks answered %d after %s, want 200 in about 300 ms", code, elapsed)
	}

	hung := func(ctx context.Context) (string, string) {
		<-ctx.Done()
		return StateUnavailable, "no answer in time"
	}
	started = time.Now()
	code, status, _ := readinessOf(t, []Check{{Name: "db", Critical: true, Probe: hung}})
	elapsed := time.Since(started)
	if code != http.StatusServiceUnavailable || status.Checks["db"] != StateUnavailable {
		t.Fatalf("hung database answered %d %v", code, status.Checks)
	}
	if elapsed < readinessTimeout || elapsed > readinessTimeout+500*time.Millisecond {
		t.Fatalf("hung database answered after %s, want the %s bound", elapsed, readinessTimeout)
	}
	if readinessTimeout >= 3*time.Second {
		t.Fatalf("readiness bound %s is not below the Helm probe timeout (3s)", readinessTimeout)
	}
}

type monitorLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *monitorLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *monitorLog) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		out = append(out, entry)
	}
	return out
}

func TestMonitorLogsOneLossAndOneRecoveryPerOutage(t *testing.T) {
	var mu sync.Mutex
	dbState, natsState := StateOK, StateOK
	probe := func(state *string) func(context.Context) (string, string) {
		return func(context.Context) (string, string) {
			mu.Lock()
			defer mu.Unlock()
			if *state == StateOK {
				return StateOK, ""
			}
			return *state, "the database system is in recovery mode (SQLSTATE 57P03)"
		}
	}
	set := func(state *string, value string) {
		mu.Lock()
		*state = value
		mu.Unlock()
	}
	checks := []Check{
		{Name: "db", Critical: true, Probe: probe(&dbState), Lost: "Main lost its database", Recovered: "Main reached its database again"},
		{Name: "nats", Probe: probe(&natsState), Lost: "Main lost NATS", Recovered: "Main reached NATS again"},
		{Name: "runners", Probe: fixed(StateDegraded, "restarted")}, // logs nothing: restarts are logged by the supervisor
	}
	logs := &monitorLog{}
	monitor, err := NewMonitor(checks, slog.New(slog.NewJSONHandler(logs, nil)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_800_000_000, 0)
	monitor.now = func() time.Time { return clock }
	evaluate := func() {
		monitor.Observe(Evaluate(context.Background(), checks))
		clock = clock.Add(time.Second)
	}

	evaluate()
	set(&dbState, StateUnavailable)
	for range 20 { // twenty failing evaluations, as twenty runners or probes would see
		evaluate()
	}
	set(&natsState, StateReconnecting)
	evaluate()
	set(&dbState, StateOK)
	evaluate()
	evaluate()
	set(&natsState, StateOK)
	evaluate()

	lines := logs.lines(t)
	want := []struct{ level, msg, dependency string }{
		{"WARN", "Main lost its database", "db"},
		{"WARN", "Main lost NATS", "nats"},
		{"INFO", "Main reached its database again after 21s", "db"},
		{"INFO", "Main reached NATS again after 3s", "nats"},
	}
	if len(lines) != len(want) {
		t.Fatalf("log lines = %v, want %d", lines, len(want))
	}
	for i, w := range want {
		if lines[i]["level"] != w.level || lines[i]["msg"] != w.msg || lines[i]["dependency"] != w.dependency {
			t.Fatalf("line %d = %v, want %v", i, lines[i], w)
		}
	}
	if lines[0]["cause"] != "the database system is in recovery mode (SQLSTATE 57P03)" {
		t.Fatalf("loss line cause = %v", lines[0]["cause"])
	}
	if lines[2]["outage"] != "21s" {
		t.Fatalf("recovery line outage = %v", lines[2]["outage"])
	}
}

func TestMonitorRunEvaluatesOnItsOwnUntilItsOwnerEnds(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	checks := []Check{{Name: "db", Critical: true, Probe: func(context.Context) (string, string) {
		mu.Lock()
		calls++
		mu.Unlock()
		return StateOK, ""
	}}}
	monitor, err := NewMonitor(checks, slog.New(slog.DiscardHandler), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { monitor.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop with its owner")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("monitor evaluated %d times in 60 ms at a 10 ms interval", calls)
	}
}

func TestReadinessIsNotReadyWhenAnyCheckCannotComeBackOnItsOwn(t *testing.T) {
	code, status, _ := readinessOf(t, []Check{{Name: "db", Critical: true, Probe: fixed(StateOK, "")}, {Name: "nats", Probe: fixed(StateUnavailable, "closed")}})
	if code != http.StatusServiceUnavailable || status.Status != "not_ready" || status.Checks["nats"] != StateUnavailable {
		t.Fatalf("readiness with a closed NATS connection = %d %+v, want 503 not_ready", code, status)
	}
}

func TestMonitorIgnoresAnEvaluationOlderThanTheOneItApplied(t *testing.T) {
	var mu sync.Mutex
	state := StateUnavailable
	checks := []Check{{Name: "db", Critical: true, Probe: func(context.Context) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return state, "the connection was refused"
	}, Lost: "Main lost its database", Recovered: "Main reached its database again"}}
	logs := &monitorLog{}
	monitor, err := NewMonitor(checks, slog.New(slog.NewJSONHandler(logs, nil)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	lost := Evaluate(context.Background(), checks)
	stale := Evaluate(context.Background(), checks) // started while the database was down
	mu.Lock()
	state = StateOK
	mu.Unlock()
	recovered := Evaluate(context.Background(), checks)

	monitor.Observe(lost)
	monitor.Observe(recovered)
	monitor.Observe(stale) // finishes last, started before the recovery
	lines := logs.lines(t)
	if len(lines) != 2 || lines[0]["level"] != "WARN" || lines[1]["level"] != "INFO" {
		t.Fatalf("log = %v, want one loss and one recovery; the stale evaluation must not log a second loss", lines)
	}
}

func TestReadinessDoesNotReportACancelledProbeAsAnOutage(t *testing.T) {
	checks := []Check{{Name: "db", Critical: true, Probe: func(ctx context.Context) (string, string) {
		<-ctx.Done()
		return StateUnavailable, "the work was cancelled"
	}, Lost: "Main lost its database", Recovered: "Main reached its database again"}}
	logs := &monitorLog{}
	monitor, err := NewMonitor(checks, slog.New(slog.NewJSONHandler(logs, nil)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	RoutesWithDeps(Deps{Checks: checks, Monitor: monitor}).ServeHTTP(httptest.NewRecorder(), request)
	if lines := logs.lines(t); len(lines) != 0 {
		t.Fatalf("a probe that went away was logged as an outage: %v", lines)
	}
}
