// overhead_loadtest.js — k6 load test for gateway-hop overhead measurement
//
// Gate: BFF.9d (overhead-check subcommand).
// Spec: §2.4 — the gateway forwarding hop must not add more than 50 ms p99 to
// the round-trip measured at the caller.
//
// Usage (against a staging deployment's public /llm):
//
//   API_KEY=<personal-access-token> k6 run \
//     --summary-export summary.json \
//     -e BASE_URL=https://staging.elitea.example \
//     -e PROJECTS=<project-id>[,<project-id>...] \
//     testdata/overhead_loadtest.js
//
// The gateway serves /llm only as GatewayService over gRPC with mutual TLS
// (#1359), so this script — HTTP — loads elitea-main's public /llm with a
// personal access token, as an external caller does. X-Elapsed-Ms is stamped
// by the gateway's own handler and reaches the caller through the edge
// unchanged, so the metric below is still the gateway's own overhead.
//
// Then feed the export to the gate:
//
//   cutover-ctl overhead-check \
//     --summary summary.json \
//     --max-p99-overhead-ms 50
//
// What this script measures:
//   - It sends non-streaming POST /llm/v1/chat/completions requests with a
//     trivial prompt to minimise model-side latency variance.
//   - After each HTTP 200 response it reads the gateway's X-Elapsed-Ms response
//     header (the gateway's own overhead up to the instant it holds the
//     provider credential, set by its /llm handler — setElapsedHeader in
//     services/elitea-llm-gateway/internal/llmproxy/httpio.go). Non-200 responses are NOT
//     recorded: an error path's latency is not a measurement of the hop, and a
//     run full of fast 4xx/5xx responses must not look like a fast gateway.
//   - Each X-Elapsed-Ms value is recorded into the custom Trend metric
//     "gateway_overhead_ms". The overhead-check gate reads exactly this metric's
//     p(99) from the exported summary.
//   - If X-Elapsed-Ms is absent (e.g. a hop on the way strips it), the script
//     falls back to recording the full round-trip AND increments the
//     "gateway_overhead_fallback" counter. The gate fails on a non-zero counter
//     unless --allow-roundtrip-fallback is passed, so this degradation can never
//     pass silently.
//
// Load profile: 60 seconds total — 10 s ramp-up to 50 VUs, 40 s steady state,
// 10 s ramp-down. Typical staging run produces ~3 000 iterations.
//
// Thresholds embedded in the script give k6 a local pass/fail signal; the gate
// provides the canonical CI assertion from the exported summary file.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import exec from 'k6/execution';

// gatewayOverheadMs records the gateway-internal hop latency (milliseconds)
// from the X-Elapsed-Ms response header, for successful responses only. This is
// the metric the overhead-check gate primarily reads.
const gatewayOverheadMs = new Trend('gateway_overhead_ms', true);

// gatewayOverheadFallback counts the samples that came from the round-trip
// duration instead of the gateway's own header. Any non-zero value means the
// trend above is NOT hop-only; the gate treats it as a failure unless the
// operator opted in with --allow-roundtrip-fallback.
const gatewayOverheadFallback = new Counter('gateway_overhead_fallback');

// ── Configuration ────────────────────────────────────────────────────────────
const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8080').replace(/\/+$/, '');
const API_KEY  = __ENV.API_KEY || '';
const MODEL    = __ENV.MODEL   || 'gpt-4o-mini';

// Request body: non-streaming, minimal prompt to keep upstream latency low.
const REQUEST_BODY = JSON.stringify({
  model: MODEL,
  stream: false,
  max_tokens: 8,
  messages: [{ role: 'user', content: 'Reply with one word: OK' }],
});

const HEADERS = {
  'Content-Type': 'application/json',
  'Authorization': `Bearer ${API_KEY}`,
};

// PROJECTS (comma-separated project ids) spreads the load across many
// (project, model) tuples, each sent as the edge's X-Project-Id selector; the
// token owner must be a member of every one. Spreading is REQUIRED against a
// production gateway: the §2.6 circular-routing breaker opens a tuple's
// circuit at >=5 req/1s, so a single-project 50-VU run measures 429s, not
// overhead. Each VU keeps ONE project (vu id modulo the set) and paces itself
// below the breaker threshold (see sleep() in the scenario). Empty = the token
// owner's personal project.
const PROJECTS = (__ENV.PROJECTS || '').split(',').map((p) => p.trim()).filter((p) => p !== '');

function projectHeaders() {
  if (PROJECTS.length === 0) return {};
  return { 'X-Project-Id': PROJECTS[(exec.vu.idInTest - 1) % PROJECTS.length] };
}

// ── Load profile ─────────────────────────────────────────────────────────────
export const options = {
  stages: [
    { duration: '10s', target: 50 },  // ramp-up
    { duration: '40s', target: 50 },  // steady state
    { duration: '10s', target: 0  },  // ramp-down
  ],
  // Local thresholds for k6's own pass/fail output. The canonical CI assertion
  // is the overhead-check gate reading the exported summary; these provide an
  // early local signal.
  thresholds: {
    // Custom metric: p99 hop overhead under 50 ms.
    gateway_overhead_ms: ['p(99)<50'],
    // Sanity: overall round-trip p99 under 2 s (model latency included).
    http_req_duration: ['p(99)<2000'],
    // Error rate must stay below 1%.
    http_req_failed: ['rate<0.01'],
  },
};

// ── Virtual user scenario ─────────────────────────────────────────────────────
export default function () {
  const res = http.post(
    `${BASE_URL}/llm/v1/chat/completions`,
    REQUEST_BODY,
    { headers: Object.assign({}, HEADERS, projectHeaders()), timeout: '10s' },
  );

  // Basic response checks.
  check(res, {
    'status is 200': (r) => r.status === 200,
    'body is JSON': (r) => {
      try { JSON.parse(r.body); return true; } catch { return false; }
    },
  });

  // Record gateway-hop overhead from the X-Elapsed-Ms header — for HTTP 200
  // only. Timing an error response measures how fast the gateway said "no",
  // which would let a wholly failing run report an excellent p99.
  // The gateway sets this header to its internal elapsed time in milliseconds
  // (float, e.g. "12.34"). If absent (stripped on the way), fall back to the
  // full round-trip duration so the test still records a conservative bound.
  if (res.status === 200) {
    const elapsedHeader = res.headers['X-Elapsed-Ms'] || res.headers['x-elapsed-ms'];
    const ms = elapsedHeader === undefined ? NaN : parseFloat(elapsedHeader);
    if (!isNaN(ms)) {
      gatewayOverheadMs.add(ms);
    } else {
      // Fallback: use the k6-measured round-trip duration as a conservative
      // proxy, and say so in the summary. This is larger than the true hop
      // overhead (it includes network RTT to the test runner), so the counter
      // makes the gate fail loudly instead of quietly grading the network.
      gatewayOverheadMs.add(res.timings.duration);
      gatewayOverheadFallback.add(1);
    }
  }

  // Pace each VU below the §2.6 loop-breaker threshold for its own
  // (project, model) tuple (>=5 req/1s opens the circuit): 0.3 s floor keeps a
  // VU at ~<3.5 req/s even when the model answers instantly.
  sleep(0.3);
}

// ── Setup / teardown (optional) ───────────────────────────────────────────────
export function setup() {
  if (API_KEY === '') {
    throw new Error('API_KEY is not set: /llm authenticates every caller with a personal access token.');
  }
  // Verify the token reaches the gateway before the test starts: one models
  // listing, which crosses the same edge and GatewayService hop as the load.
  // setup() runs outside any VU, so it names the first project directly.
  const probe = http.get(`${BASE_URL}/llm/v1/models`, {
    headers: Object.assign({}, HEADERS, PROJECTS.length ? { 'X-Project-Id': PROJECTS[0] } : {}),
    timeout: '10s',
  });
  if (probe.status !== 200) {
    throw new Error(
      `/llm/v1/models answered ${probe.status}. ` +
      `Is BASE_URL=${BASE_URL} correct and API_KEY valid for the project(s)?`
    );
  }
  console.log(`/llm reachable at ${BASE_URL}. Starting overhead load test.`);
}
