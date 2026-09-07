package main

import (
	"fmt"
	"os"
)

// cutover-ctl is the operator command line for the LLM-path cutover gates.
//
// It KEEPS the seven verification subcommands below. Each one drives a real
// deployment or a recorded fixture and reports a pass or a fail: they are the
// acceptance harness for the LiteLLM-to-Bifrost move (BFC/BF work packages).
//
// It LOSES the endpoint-promotion half — `status`, `summary`, `promote`,
// `promote-all`, `rollback` and `decommission-check`. Those six were clients
// of `/internal/cutover` and `/internal/shadow` on elitea-main. #383 deleted
// the handlers behind both mounts, together with the Redis endpoint-state
// tracker and the pylon reverse proxy they drove, because no composition root
// ever populated them: the routes answered 404 in every deployment, so the six
// subcommands could never have promoted anything. They are removed rather than
// repaired — the traffic split they managed is finished, and elitea-main no
// longer proxies to pylon at all.
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "budget-status-audit":
		cmdBudgetStatusAudit(os.Args[2:])
	case "sse-flush-check":
		cmdSSEFlushCheck(os.Args[2:])
	case "cost-parity":
		cmdCostParity(os.Args[2:])
	case "models-parity":
		cmdModelsParity(os.Args[2:])
	case "overhead-check":
		cmdOverheadCheck(os.Args[2:])
	case "budget-check":
		cmdBudgetCheck(os.Args[2:])
	case "cutover-verify":
		cmdCutoverVerify(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `cutover-ctl — EliteA cutover verification tool

Commands:
  budget-status-audit  Scan --paths for budget/quota call sites that handle 429 but not 402
  sse-flush-check      Assert the gateway streams SSE incrementally for both dialects
  cost-parity          Assert gateway nano-USD cost math matches the pylon CostCalculator
  models-parity        Assert gateway /llm/v1/models set-equivalent to legacy for N projects (p99 < M ms)
  overhead-check       Drive the k6 load script (or parse --summary) and assert gateway-hop p99 < threshold; persists testdata/p99_overhead_benchmark.json
  budget-check         Assert over-budget returns 402 type=budget_exceeded/code=insufficient_quota + soft-alert within latency window
  cutover-verify       Post-cutover gate BFC.9: assert zero 5xx, no litellm subprocess, zero legacy traffic, 402 block confirmed

Options:
  --paths <roots>      (budget-status-audit) comma-separated roots to scan
  --gateway-url <url>  (sse-flush-check) gateway base URL (default http://localhost:8083)
  --against <ref>      (cost-parity) reference cost model to compare against (default pylon)
  --min-projects <N>   (models-parity) minimum projects to check (default 5)
  --max-p99-ms <M>     (models-parity) maximum acceptable p99 gateway /v1/models latency (default 200)
  --projects-file <f>        (models-parity) seeded projects fixture (JSON array); (budget-check) seeded {over_budget, soft_alert, under_budget} fixture
  --max-p99-overhead-ms <N>  (overhead-check) maximum acceptable gateway-hop p99 latency in ms (default 50)
  --summary <path>           (overhead-check) pre-exported k6 --summary-export JSON; skips running k6 (hermetic mode)
  --script <path>            (overhead-check) k6 load script to drive (default testdata/overhead_loadtest.js)
  --k6-bin <path>            (overhead-check) k6 binary to invoke (default k6)
  --benchmark-out <path>     (overhead-check) benchmark record output (default testdata/p99_overhead_benchmark.json; "" disables)
  --alert-latency-s <N>      (budget-check) max seconds from 80%% crossing to soft-alert observation (default 10)
  --nats-url <url>           (budget-check) NATS URL for the gateway.events.* subscription (default nats://localhost:4222)
  --identity-secret <s>      (budget-check) edge identity HMAC secret (default $GATEWAY_IDENTITY_SECRET)
  --deploy <name>            (cutover-verify) deployment name to inspect (live mode, default elitea-main)
  --port <N>                 (cutover-verify) gateway port for HTTP probe (live mode, default 8083)
  --litellm-svc <addr>       (cutover-verify) legacy LiteLLM service address (live mode, default litellm-svc:4000)
  --window-m <N>             (cutover-verify) Prometheus observation window in minutes (default 15)
  --fixture <path>           (cutover-verify) JSON cutoverState fixture file for hermetic verification (skips live cluster queries)
`)
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
