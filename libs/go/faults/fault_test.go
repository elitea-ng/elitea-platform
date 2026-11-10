package faults

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type secretError struct{}

func (secretError) Error() string {
	return "dial postgres://admin:hunter2@db.internal:5432/elitea failed"
}

func TestCauseChainIsBoundedAndUsesTheDescriberForKnownTypes(t *testing.T) {
	inner := secretError{}
	err := fmt.Errorf("verify session: %w", fmt.Errorf("begin: %w", inner))
	describe := func(e error) (string, bool) {
		if _, ok := e.(secretError); ok {
			return "the connection was refused", true
		}
		return "", false
	}
	chain := CauseChain(err, describe)
	if strings.Join(chain, " | ") != "verify session | begin | the connection was refused" {
		t.Fatalf("chain = %q", chain)
	}
	if strings.Contains(strings.Join(chain, " "), "hunter2") {
		t.Fatal("a described type leaked its text")
	}

	deep := errors.New("root")
	for i := range 20 {
		deep = fmt.Errorf("layer %d: %w", i, deep)
	}
	if got := CauseChain(deep, nil); len(got) != maxCauseDepth {
		t.Fatalf("chain depth = %d, want %d", len(got), maxCauseDepth)
	}
	long := errors.New(strings.Repeat("é", 400))
	if got := CauseChain(long, nil); len(got[0]) > maxCauseBytes+len("…") {
		t.Fatalf("element of %d bytes, over the bound", len(got[0]))
	}
}

func TestATypedFaultRendersAsItsCodeAndSentence(t *testing.T) {
	err := fmt.Errorf("list tools: %w", New(ToolkitSettingsUnresolved, errors.New("toolkit settings resolution is unavailable")))
	chain := CauseChain(err, nil)
	want := []string{"list tools", "toolkit.settings_unresolved: " + ToolkitSettingsUnresolved.Sentence(), "toolkit settings resolution is unavailable"}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Fatalf("chain = %q, want %q", chain, want)
	}
}

func TestUnexpectedCarriesABoundedRateLimitedStack(t *testing.T) {
	stackMu.Lock()
	clock := time.Unix(1_800_000_000, 0)
	nowFunc = func() time.Time { return clock }
	lastStack = map[Code]time.Time{}
	stackMu.Unlock()
	defer func() { nowFunc = time.Now }()

	first := Unexpected(errors.New("boom"))
	if first.Code() != InternalUnexpected || first.Stack() == "" || first.Stack() == "rate-limited" {
		t.Fatalf("first unexpected = %v stack=%q", first.Code(), first.Stack())
	}
	if len(first.Stack()) > maxStackBytes+8 {
		t.Fatalf("stack of %d bytes, over the bound", len(first.Stack()))
	}
	if !strings.Contains(first.Stack(), "TestUnexpectedCarriesABoundedRateLimitedStack") {
		t.Fatalf("the stack does not name where it was raised:\n%s", first.Stack())
	}
	if second := Unexpected(errors.New("boom")); second.Stack() != "rate-limited" {
		t.Fatalf("a second stack within a second = %q, want rate-limited", second.Stack())
	}
	stackMu.Lock()
	clock = clock.Add(stackRateLimitSpan)
	stackMu.Unlock()
	if third := Unexpected(errors.New("boom")); third.Stack() == "rate-limited" {
		t.Fatal("the rate limit did not end after a second")
	}
}

func TestPanicNeverRendersThePanicValue(t *testing.T) {
	fault := Panic("token=sk-live-123 for user ada@example.com")
	text := fault.Error() + strings.Join(CauseChain(fault, nil), " ")
	for _, leak := range []string{"sk-live", "ada@example.com", "token="} {
		if strings.Contains(text, leak) {
			t.Fatalf("the panic value leaked: %q", text)
		}
	}
	if fault.Code() != InternalPanic || !strings.Contains(text, "string") {
		t.Fatalf("panic fault = %v %q", fault.Code(), text)
	}
}

func TestTransitionsReportEachOutageOnce(t *testing.T) {
	transitions := NewTransitions()
	clock := time.Unix(1_800_000_000, 0)
	transitions.now = func() time.Time { return clock }
	if got, _ := transitions.Observe("db", true); got != Lost {
		t.Fatalf("first failure = %v, want Lost", got)
	}
	for range 10 {
		if got, _ := transitions.Observe("db", true); got != Unchanged {
			t.Fatalf("repeated failure = %v, want Unchanged", got)
		}
	}
	if !transitions.IsLost("db") || transitions.IsLost("nats") {
		t.Fatal("IsLost does not follow the outage")
	}
	clock = clock.Add(45 * time.Second)
	got, outage := transitions.Observe("db", false)
	if got != Back || outage != 45*time.Second || transitions.IsLost("db") {
		t.Fatalf("recovery = %v after %s", got, outage)
	}
}

type lineLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lineLog) lines(t *testing.T) []map[string]any {
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

func countLevel(entries []map[string]any, level string) int {
	n := 0
	for _, e := range entries {
		if e["level"] == level {
			n++
		}
	}
	return n
}

// C3, first half: during a reported database outage, a runner's database
// failures are DEBUG; the one outage line says it.
func TestCycleReporterIsQuietOnlyWhileTheOutageIsReported(t *testing.T) {
	logs := &lineLog{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	outages := NewTransitions()
	dbDown := errors.New("FATAL: server login has been failing (SQLSTATE 08P01)")
	reporter := NewCycleReporter("runtime outbox publisher", logger, outages, func(err error) string {
		if err == dbDown {
			return "db"
		}
		return ""
	})
	outages.Observe("db", true)
	for range 50 {
		reporter.Report(dbDown)
	}
	entries := logs.lines(t)
	if countLevel(entries, "ERROR") != 0 || countLevel(entries, "DEBUG") != 50 {
		t.Fatalf("during the reported outage: %d ERROR, %d DEBUG; want 0 and 50", countLevel(entries, "ERROR"), countLevel(entries, "DEBUG"))
	}
}

// C3, second half: the same transient-looking failure with no reported
// outage (a query that times out every cycle on a healthy database) is an
// ERROR once a minute, with the suppressed count; it is never hidden.
func TestCycleReporterLogsARepeatingFailureOnceAMinuteWithItsCount(t *testing.T) {
	logs := &lineLog{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	timeout := fmt.Errorf("claim pending effects: %w", errors.New("context deadline exceeded"))
	reporter := NewCycleReporter("index metadata terminal reconciler", logger, NewTransitions(), func(error) string { return "db" })
	clock := time.Unix(1_800_000_000, 0)
	reporter.now = func() time.Time { return clock }
	for range 4 { // four minutes of a failure every 10 s
		for range 6 {
			reporter.Report(timeout)
			clock = clock.Add(10 * time.Second)
		}
	}
	entries := logs.lines(t)
	if countLevel(entries, "DEBUG") != 0 {
		t.Fatal("a failure with no reported outage was demoted to DEBUG")
	}
	if countLevel(entries, "ERROR") != 4 {
		t.Fatalf("ERROR lines = %d, want 4 (once a minute)", countLevel(entries, "ERROR"))
	}
	if entries[0]["suppressed"] != float64(0) || entries[1]["suppressed"] != float64(5) {
		t.Fatalf("suppressed counts = %v, %v; want 0 then 5", entries[0]["suppressed"], entries[1]["suppressed"])
	}
	if entries[0]["msg"] != RunnerCycleFailed.Sentence() || entries[0]["runner"] != "index metadata terminal reconciler" {
		t.Fatalf("line = %v", entries[0])
	}
}

// C3: a runner's first failure in an outage asks the owner to check now, so
// it finds the outage reported instead of racing the monitor's interval.
func TestTheFirstFailureOfAnOutageAsksTheOwnerToConfirmIt(t *testing.T) {
	logs := &lineLog{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	outages := NewTransitions()
	databaseDown := true
	confirmations := 0
	outages.SetConfirm(func(name string) {
		confirmations++
		outages.Observe(name, databaseDown)
	})
	reporter := NewCycleReporter("agent outbox publisher", logger, outages, func(error) string { return "db" })
	reporter.Report(errors.New("connection refused"))
	if confirmations != 1 || countLevel(logs.lines(t), "ERROR") != 0 || countLevel(logs.lines(t), "DEBUG") != 1 {
		t.Fatalf("confirmations=%d lines=%v; want the outage confirmed and the failure at DEBUG", confirmations, logs.lines(t))
	}

	// A healthy dependency (a slow query timing out) is confirmed healthy,
	// and the failure stays an ERROR.
	databaseDown = false
	outages.Observe("db", false)
	reporter.Report(errors.New("context deadline exceeded"))
	if countLevel(logs.lines(t), "ERROR") != 1 {
		t.Fatalf("a failure on a healthy database was not an ERROR: %v", logs.lines(t))
	}
}
