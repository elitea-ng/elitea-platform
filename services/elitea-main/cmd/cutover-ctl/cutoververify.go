package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// cutover-verify (spec BFC.9, post-cutover gate):
//
// After the big-bang cutover this gate confirms that the migration is truly
// complete and no legacy path remains active. It checks three invariants:
//
//  1. Zero gateway 5xx responses over the observation window: the gateway must
//     be stable. Any 5xx indicates a broken route that needs attention.
//  2. Zero traffic to litellm-svc:4000: the internal LiteLLM service must be
//     receiving no traffic. Any positive RPS means callers are still hitting the
//     legacy path.
//  3. The 402-hard-block is confirmed: the gateway must respond 402 Payment
//     Required when the project budget is exhausted. Without this, quota
//     enforcement is broken post-cutover.
//
// # Real (live) mode
//
// The real gate gathers state from live cluster sources:
//   - Prometheus: query rate(elitea_gateway_http_requests_total{status=~"5.."}[Wm]) == 0
//   - Prometheus: query rate(elitea_litellm_svc_requests_total[Wm]) == 0
//   - HTTP probe: POST /llm/v1/chat/completions with an exhausted budget → expect 402
//
// Flags for live mode: --deploy, --port, --litellm-svc, --window-m
//
// # Hermetic (fixture) mode
//
// For CI and offline verification, pass --fixture <path> to a JSON file
// encoding a cutoverState struct. The gate evaluates the check logic against
// the fixture data and exits 0/non-zero accordingly. This exercises the exact
// same evaluateCutover function that the live mode uses — the only difference
// is how the state is populated.
//
// See testdata/cutover_state_clean.json  (should pass)
// See testdata/cutover_state_dirty.json  (should fail)

// cutoverState is the injectable state struct used by both the live and fixture
// paths. In a real run each field is populated by querying k8s/Prometheus/HTTP;
// in a hermetic run it is loaded from a JSON fixture file.
type cutoverState struct {
	// Gateway5xxCount is the total number of 5xx responses observed at the
	// gateway over the observation window. Must be zero for the gate to pass.
	Gateway5xxCount int `json:"gateway_5xx_count"`

	// LegacySvcTrafficRPS is the observed traffic rate to litellm-svc:4000 in
	// requests per second. Must be exactly 0.0 for the gate to pass.
	LegacySvcTrafficRPS float64 `json:"legacy_svc_traffic_rps"`

	// HardBlock402Confirmed is true if the gateway returns HTTP 402 when a
	// project budget is exhausted. Must be true for the gate to pass.
	HardBlock402Confirmed bool `json:"hard_block_402_confirmed"`

	// WindowMinutes is the observation window duration used for Prometheus
	// range queries (informational; does not affect the pass/fail logic).
	WindowMinutes int `json:"window_minutes"`
}

// evaluateCutover is the pure check function. It accepts a populated
// cutoverState and returns (ok, reasons). ok is true only when all three
// invariants hold. reasons lists one human-readable sentence per failed check;
// it is empty when ok is true.
//
// This function has no side effects (no I/O, no os.Exit) so it can be
// called directly from tests.
func evaluateCutover(state cutoverState) (ok bool, reasons []string) {
	if state.Gateway5xxCount != 0 {
		reasons = append(reasons, fmt.Sprintf(
			"gateway 5xx count is %d over the window (want 0); investigate broken routes before declaring cutover clean",
			state.Gateway5xxCount,
		))
	}
	if state.LegacySvcTrafficRPS != 0 {
		reasons = append(reasons, fmt.Sprintf(
			"litellm-svc:4000 is receiving %.4f RPS (want 0); callers are still routing to the legacy path",
			state.LegacySvcTrafficRPS,
		))
	}
	if !state.HardBlock402Confirmed {
		reasons = append(reasons, "402 hard-block is NOT confirmed; the gateway did not return 402 for an exhausted budget — "+
			"quota enforcement is broken post-cutover")
	}
	ok = len(reasons) == 0
	return ok, reasons
}

// loadCutoverStateFromFile reads and deserialises a JSON fixture file into a
// cutoverState. It is used by both the hermetic test path and the --fixture CLI
// flag. It returns an error if the file cannot be read or the JSON is malformed.
func loadCutoverStateFromFile(path string) (cutoverState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cutoverState{}, fmt.Errorf("cutover-verify: cannot read fixture %q: %w", path, err)
	}
	// Unknown fields are refused: a state recorded for an invariant the gate
	// no longer checks must fail loudly, not pass with the field ignored.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var state cutoverState
	if err := dec.Decode(&state); err != nil {
		return cutoverState{}, fmt.Errorf("cutover-verify: malformed fixture JSON %q: %w", path, err)
	}
	return state, nil
}

// cmdCutoverVerify is the `cutover-ctl cutover-verify` entrypoint (gate BFC.9).
//
// In fixture mode (--fixture <path>) it loads state from the JSON file and
// evaluates it hermetially — no network, no k8s access. This is the path used
// in CI and for offline runbook verification.
//
// In live mode (no --fixture) it would gather state from the running cluster
// using --deploy, --port, --litellm-svc, and --window-m; that path is stubbed
// with a clear error so operators know it requires implementation wiring for
// their specific cluster toolchain.
//
// Exit codes:
//
//	0  — all three invariants hold; cutover is clean
//	1  — one or more invariants failed; reasons listed on stderr
//	2  — flag/usage error or fixture load failure
func cmdCutoverVerify(args []string) {
	fs := flag.NewFlagSet("cutover-verify", flag.ExitOnError)
	deploy := fs.String("deploy", "elitea-main", "deployment name to inspect (live mode)")
	port := fs.Int("port", 8083, "gateway port for HTTP probe (live mode)")
	litellmSvc := fs.String("litellm-svc", "litellm-svc:4000", "legacy LiteLLM service address to check (live mode)")
	windowM := fs.Int("window-m", 15, "Prometheus observation window in minutes")
	fixturePath := fs.String("fixture", "", "path to a JSON cutoverState fixture file (hermetic mode; skips live cluster queries)")
	_ = fs.Parse(args)

	var state cutoverState
	var err error

	if *fixturePath != "" {
		// Hermetic fixture mode: load state from JSON, no cluster access.
		state, err = loadCutoverStateFromFile(*fixturePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		fmt.Printf("cutover-verify: fixture mode — loaded state from %q\n", *fixturePath)
	} else {
		// Live mode: gather state from the running cluster.
		//
		// NOTE: This path is the real gate. It requires cluster access and
		// Prometheus queries that are specific to your deployment environment.
		// The implementation below is a clear stub so operators know exactly
		// which data to wire in. Replace each TODO with the real query.
		//
		// The live gathering logic intentionally lives here (not in
		// evaluateCutover) so the pure check function remains testable without
		// any I/O dependency.
		fmt.Printf("cutover-verify: live mode — deploy=%s port=%d litellm-svc=%s window=%dm\n",
			*deploy, *port, *litellmSvc, *windowM)
		fmt.Fprintln(os.Stderr, "cutover-verify: live mode is not yet wired to a cluster client.\n"+
			"  Use --fixture <path> for hermetic verification, or implement the Prometheus/k8s\n"+
			"  queries below for your environment (see the TODO markers in cutoververify.go).")
		// TODO: query Prometheus for gateway 5xx count over the window.
		// TODO: query Prometheus for litellm-svc:4000 traffic RPS.
		// TODO: HTTP probe against gateway for 402 response with exhausted budget.
		os.Exit(2)
	}

	ok, reasons := evaluateCutover(state)

	// Print a summary for operator visibility regardless of outcome.
	fmt.Printf("cutover-verify: window=%dm  5xx=%d  legacy_rps=%.4f  402_confirmed=%v\n",
		state.WindowMinutes,
		state.Gateway5xxCount,
		state.LegacySvcTrafficRPS,
		state.HardBlock402Confirmed,
	)

	if ok {
		fmt.Println("✓ cutover-verify: all post-cutover invariants hold — cutover is clean (gate BFC.9)")
		return
	}

	fmt.Fprintf(os.Stderr, "\n✗ cutover-verify: %d invariant(s) failed (gate BFC.9):\n", len(reasons))
	for i, r := range reasons {
		fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, r)
	}
	os.Exit(1)
}
