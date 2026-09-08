package main

// budget_startup_gate_test.go — issue #304.
//
// The suite covers the three surfaces the gate is built from, because a green
// test of any one of them alone would still let the whole gate be off:
//
//   - the DECISION (budgetStartupRefusal), over the full mode × state matrix;
//   - the PROBE (probeAuthoredBudgetsWith), including the two ways it must NOT
//     answer "no budgets" — a missing table it never queried, and a database
//     error it could not read;
//   - the PROCESS (TestGatewayRefusesToStartWithoutEnforcement), which runs the
//     real main() in a subprocess and reads its exit status, because "exits
//     non-zero" is the acceptance criterion and no unit test of a function can
//     prove it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
)

// ---- the decision -----------------------------------------------------------

func TestBudgetStartupRefusal_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		natsURL  string
		wired    bool
		budgets  budgetEvidence
		wantStop bool
		wantSays string
	}{
		{
			name:  "a wired gateway starts in every mode",
			mode:  config.RequireEnforcementOn,
			wired: true,
		},
		{
			name:    "auto starts when NATS is configured but the dial failed",
			mode:    config.RequireEnforcementAuto,
			natsURL: "nats://nats:4222",
			budgets: budgetEvidence{found: true},
			// /readyz already reports not ready and startBudgetRecovery
			// re-dials (issue #315). Refusing here would trade a pod that
			// recovers on its own for a CrashLoopBackOff.
		},
		{
			name:     "auto REFUSES with no NATS URL and an authored budget",
			mode:     config.RequireEnforcementAuto,
			budgets:  budgetEvidence{found: true},
			wantStop: true,
			wantSays: "GATEWAY_NATS_URL is empty",
		},
		{
			name:    "auto starts with no NATS URL and no authored budget",
			mode:    config.RequireEnforcementAuto,
			budgets: budgetEvidence{},
		},
		{
			name:    "auto starts when the probe could not answer",
			mode:    config.RequireEnforcementAuto,
			budgets: budgetEvidence{err: errNoBudgetPool},
		},
		{
			name:     "on REFUSES an unwired gate with NATS configured",
			mode:     config.RequireEnforcementOn,
			natsURL:  "nats://nats:4222",
			wantStop: true,
			wantSays: "LLM_BUDGET_REQUIRE_ENFORCEMENT=on",
		},
		{
			name:     "on REFUSES an unwired gate with nothing configured at all",
			mode:     config.RequireEnforcementOn,
			wantStop: true,
			wantSays: "LLM_BUDGET_REQUIRE_ENFORCEMENT=on",
		},
		{
			name:    "off starts in the state auto refuses",
			mode:    config.RequireEnforcementOff,
			budgets: budgetEvidence{found: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{NATSURL: tc.natsURL, RequireBudgetEnforcement: tc.mode}
			err := budgetStartupRefusal(cfg, tc.wired, tc.budgets)
			if tc.wantStop && err == nil {
				t.Fatal("the gate admitted a startup it must refuse: the gateway would serve /llm unmetered")
			}
			if !tc.wantStop && err != nil {
				t.Fatalf("the gate refused a supported posture: %v", err)
			}
			if tc.wantStop && !strings.Contains(err.Error(), tc.wantSays) {
				t.Fatalf("the refusal does not name the setting an operator must change: %q", err)
			}
		})
	}
}

// TestBudgetStartupRefusal_UnsetModeIsAuto proves the zero value behaves. A
// Config built in code rather than from FromEnv carries "" here, and reading
// that as the permissive mode would disarm the gate for every such caller.
func TestBudgetStartupRefusal_UnsetModeIsAuto(t *testing.T) {
	cfg := config.Config{RequireBudgetEnforcement: ""}
	if err := budgetStartupRefusal(cfg, false, budgetEvidence{found: true}); err == nil {
		t.Fatal("an empty LLM_BUDGET_REQUIRE_ENFORCEMENT was read as \"off\"")
	}
}

// TestRequireEnforcementRejectsATypo proves the env reader refuses anything but
// the three words, so "0", "false" and "no" cannot select the permissive mode
// by accident — only the word "off" can.
func TestRequireEnforcementRejectsATypo(t *testing.T) {
	for _, v := range []string{"0", "false", "no", "OFF", "disabled", ""} {
		t.Setenv("LLM_BUDGET_REQUIRE_ENFORCEMENT", v)
		got := config.FromEnv().RequireBudgetEnforcement
		if got != config.RequireEnforcementAuto {
			t.Fatalf("LLM_BUDGET_REQUIRE_ENFORCEMENT=%q read as %q, want %q", v, got, config.RequireEnforcementAuto)
		}
	}
	for _, v := range []string{config.RequireEnforcementOn, config.RequireEnforcementOff, config.RequireEnforcementAuto} {
		t.Setenv("LLM_BUDGET_REQUIRE_ENFORCEMENT", v)
		if got := config.FromEnv().RequireBudgetEnforcement; got != v {
			t.Fatalf("LLM_BUDGET_REQUIRE_ENFORCEMENT=%q read as %q", v, got)
		}
	}
}

// TestReportBudgetStartupPosture_SaysWhatItSaw covers the half of this issue
// that is not a refusal. Two of the three states start, and both of them must
// leave a line an operator can alarm on.
func TestReportBudgetStartupPosture_SaysWhatItSaw(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		budgets budgetEvidence
		wantLog string
	}{
		{
			name:    "an unreadable probe is reported as unreadable, not as no budgets",
			mode:    config.RequireEnforcementAuto,
			budgets: budgetEvidence{err: errors.New("connection refused")},
			wantLog: "BUDGET EVIDENCE UNREADABLE",
		},
		{
			name:    "the opt-out names itself",
			mode:    config.RequireEnforcementOff,
			budgets: budgetEvidence{found: true},
			wantLog: "BUDGET STARTUP GATE DISABLED",
		},
		{
			name:    "authored budgets with no gate are reported",
			mode:    config.RequireEnforcementOff,
			budgets: budgetEvidence{found: true},
			wantLog: "AUTHORED BUDGETS FOUND",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			cfg := config.Config{RequireBudgetEnforcement: tc.mode}
			if err := reportBudgetStartupPosture(cfg, logger, false, tc.budgets); err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("the startup log does not carry %q: %s", tc.wantLog, buf.String())
			}
		})
	}
}

// ---- the probe --------------------------------------------------------------

// fakeRow answers one QueryRow. A nil err scans the values in order.
type fakeRow struct {
	vals []bool
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan wants %d destinations, the row has %d", len(dest), len(r.vals))
	}
	for i, d := range dest {
		p, ok := d.(*bool)
		if !ok {
			return fmt.Errorf("destination %d is %T, want *bool", i, d)
		}
		*p = r.vals[i]
	}
	return nil
}

// fakeQuerier answers by SQL text and records what it was asked.
type fakeQuerier struct {
	rows map[string]fakeRow
	seen []string
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.seen = append(q.seen, sql)
	if r, ok := q.rows[sql]; ok {
		return r
	}
	return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
}

func TestProbeAuthoredBudgets(t *testing.T) {
	cases := []struct {
		name      string
		rows      map[string]fakeRow
		wantFound bool
		wantErr   bool
		// wantUnasked names a query the probe must NOT run.
		wantUnasked string
	}{
		{
			name: "no budget tables is a real no, and neither table is queried",
			rows: map[string]fakeRow{
				budgetTableProbeSQL: {vals: []bool{false, false}},
			},
			wantUnasked: projectBudgetProbeSQL,
		},
		{
			name: "an enforcing project row is evidence",
			rows: map[string]fakeRow{
				budgetTableProbeSQL:   {vals: []bool{true, true}},
				projectBudgetProbeSQL: {vals: []bool{true}},
				userBudgetProbeSQL:    {vals: []bool{false}},
			},
			wantFound: true,
		},
		{
			name: "a member cap alone is evidence",
			rows: map[string]fakeRow{
				budgetTableProbeSQL:   {vals: []bool{true, true}},
				projectBudgetProbeSQL: {vals: []bool{false}},
				userBudgetProbeSQL:    {vals: []bool{true}},
			},
			wantFound: true,
		},
		{
			name: "no enforcing row is a no",
			rows: map[string]fakeRow{
				budgetTableProbeSQL:   {vals: []bool{true, true}},
				projectBudgetProbeSQL: {vals: []bool{false}},
				userBudgetProbeSQL:    {vals: []bool{false}},
			},
		},
		{
			name: "a read failure is an error, NOT an absence of budgets",
			rows: map[string]fakeRow{
				budgetTableProbeSQL:   {vals: []bool{true, true}},
				projectBudgetProbeSQL: {err: errors.New("connection refused")},
			},
			wantErr: true,
		},
		{
			name: "a failure on the table probe is an error too",
			rows: map[string]fakeRow{
				budgetTableProbeSQL: {err: errors.New("connection refused")},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{rows: tc.rows}
			got := probeAuthoredBudgetsWith(context.Background(), q)
			if tc.wantErr && got.err == nil {
				t.Fatal("a failed read reported \"no budgets\", which disarms the gate on every database blip")
			}
			if !tc.wantErr && got.err != nil {
				t.Fatalf("unexpected probe error: %v", got.err)
			}
			if got.found != tc.wantFound {
				t.Fatalf("found = %v, want %v", got.found, tc.wantFound)
			}
			if tc.wantUnasked != "" {
				for _, sql := range q.seen {
					if sql == tc.wantUnasked {
						t.Fatal("the probe queried a table it had just proved does not exist; " +
							"PostgreSQL resolves relations at parse time, so that raises 42P01")
					}
				}
			}
		})
	}
}

// TestProbeAuthoredBudgets_NilPoolDoesNotPanic covers the typed-nil trap: a nil
// *pgxpool.Pool boxed into the querier interface passes a `!= nil` test and
// panics on first use. This binary has shipped that bug once already.
func TestProbeAuthoredBudgets_NilPoolDoesNotPanic(t *testing.T) {
	got := probeAuthoredBudgets(context.Background(), nil)
	if got.err == nil {
		t.Fatal("a gateway with no database pool reported that it read the budget tables")
	}
	if got.found {
		t.Fatal("a gateway with no database pool reported authored budgets")
	}
}

// ---- the process ------------------------------------------------------------

// gatewaySubprocessEnv is the marker that turns this test binary into the
// gateway itself.
const gatewaySubprocessEnv = "GATEWAY_STARTUP_GATE_SUBPROCESS"

// TestGatewayRefusesToStartWithoutEnforcement is the acceptance criterion: the
// PROCESS exits non-zero, before the listener, when enforcement is required and
// NATS cannot be reached.
//
// It re-executes this test binary with the marker set; the marked run calls the
// real main() with an unreachable NATS, no usable database pool, and
// LLM_BUDGET_REQUIRE_ENFORCEMENT=on. A function-level test cannot prove this:
// the refusal is an os.Exit in a composition root, and the failure this issue
// describes is precisely a gateway that CONTINUES past it.
func TestGatewayRefusesToStartWithoutEnforcement(t *testing.T) {
	if os.Getenv(gatewaySubprocessEnv) == "1" {
		main()
		return
	}

	command := func(t *testing.T, ctx context.Context, mode string) (*exec.Cmd, *bytes.Buffer) {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGatewayRefusesToStartWithoutEnforcement")
		cmd.Env = append(os.Environ(),
			gatewaySubprocessEnv+"=1",
			// Port 1 is reserved and refuses immediately, so the dial fails
			// fast and leaves the NATS client nil — the exact state of a
			// gateway that boots during a NATS outage.
			"GATEWAY_NATS_URL=nats://127.0.0.1:1",
			"LLM_BUDGET_REQUIRE_ENFORCEMENT="+mode,
			// Not a URL, so pgxpool.New fails and the pool is nil. That keeps
			// the run away from a live database and off the vault path, and it
			// is also the second half of the unwired state.
			"DATABASE_URL=not-a-database-url",
			// Required whenever NATS is configured (issue #11), and this test
			// must fail on the budget gate, not on that one.
			"GATEWAY_IDENTITY_SECRET=startup-gate-test",
			// Port 0 so a run that WRONGLY starts cannot collide with a real
			// listener; it is still a defect, and the assertions below catch it.
			"GATEWAY_HTTP_ADDR=127.0.0.1:0",
			"GATEWAY_LOG_LEVEL=debug",
		)
		out := &bytes.Buffer{}
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd, out
	}

	t.Run("on refuses", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd, out := command(t, ctx, config.RequireEnforcementOn)
		err := cmd.Run()
		if ctx.Err() != nil {
			t.Fatalf("the gateway kept running instead of refusing to start; output:\n%s", out.String())
		}
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("running the gateway subprocess: %v; output:\n%s", err, out.String())
		}
		if code == 0 {
			t.Fatalf("the gateway exited 0 with enforcement required and NATS unreachable — "+
				"it would serve /llm unmetered for the life of the process; output:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "FATAL: refusing to start") {
			t.Fatalf("the process exited %d without the refusal line, so it stopped for another reason; output:\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), "LLM_BUDGET_REQUIRE_ENFORCEMENT=on") {
			t.Fatalf("the refusal does not name the budget gate; output:\n%s", out.String())
		}
	})

	// The negative control. Without it the subtest above proves only that this
	// environment cannot start a gateway — a bogus DATABASE_URL and an
	// unreachable NATS would produce the same exit if the gate did nothing.
	// The SAME environment with the mode turned off must keep running.
	t.Run("off keeps serving in the same environment", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd, out := command(t, ctx, config.RequireEnforcementOff)
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting the gateway subprocess: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			t.Fatalf("the gateway exited (%v) with LLM_BUDGET_REQUIRE_ENFORCEMENT=off, so the refusal above "+
				"is not attributable to the startup gate; output:\n%s", err, out.String())
		case <-time.After(3 * time.Second):
			// Still running, which is the documented opt-out posture.
		}
		cancel()
		<-done
	})
}
