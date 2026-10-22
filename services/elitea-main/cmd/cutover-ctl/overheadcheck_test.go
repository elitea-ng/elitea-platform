package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fixture reads a testdata file relative to this test's source directory and
// returns its bytes, failing the test on any error.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %q: %v", name, err)
	}
	return data
}

// TestOverheadCheck_UnderThreshold_Pass asserts that the under-threshold fixture
// (gateway_overhead_ms p99 = 38.475 ms < 50 ms) parses successfully and yields
// Pass=true.
func TestOverheadCheck_UnderThreshold_Pass(t *testing.T) {
	data := fixture(t, "k6_summary_under_threshold.json")
	res, err := parseK6SummaryForOverhead(data, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MetricUsed != "gateway_overhead_ms" {
		t.Errorf("MetricUsed = %q, want gateway_overhead_ms", res.MetricUsed)
	}
	if res.P99MS != 38.475 {
		t.Errorf("P99MS = %v, want 38.475", res.P99MS)
	}
	if !res.Pass {
		t.Errorf("Pass = false, want true (p99=%.3f < threshold=%.0f)", res.P99MS, res.ThresholdMS)
	}
}

// TestOverheadCheck_OverThreshold_Fail asserts that the over-threshold fixture
// (gateway_overhead_ms p99 = 87.334 ms > 50 ms) parses successfully and yields
// Pass=false.
func TestOverheadCheck_OverThreshold_Fail(t *testing.T) {
	data := fixture(t, "k6_summary_over_threshold.json")
	res, err := parseK6SummaryForOverhead(data, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MetricUsed != "gateway_overhead_ms" {
		t.Errorf("MetricUsed = %q, want gateway_overhead_ms", res.MetricUsed)
	}
	if res.P99MS != 87.334 {
		t.Errorf("P99MS = %v, want 87.334", res.P99MS)
	}
	if res.Pass {
		t.Errorf("Pass = true, want false (p99=%.3f >= threshold=%.0f)", res.P99MS, res.ThresholdMS)
	}
}

// TestOverheadCheck_MalformedSummary_Fail asserts that a JSON parse error returns
// a non-nil error (no panic, no os.Exit).
func TestOverheadCheck_MalformedSummary_Fail(t *testing.T) {
	_, err := parseK6SummaryForOverhead([]byte(`not valid json {{{`), 50)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// TestOverheadCheck_NoMetricsKey_Fail asserts that a well-formed JSON object that
// lacks a "metrics" key returns an error.
func TestOverheadCheck_NoMetricsKey_Fail(t *testing.T) {
	_, err := parseK6SummaryForOverhead([]byte(`{"root_groups":{}}`), 50)
	if err == nil {
		t.Fatal("expected error for missing metrics key, got nil")
	}
}

// TestOverheadCheck_FallbackMetric_Pass asserts that when gateway_overhead_ms is
// absent the fallback metric (http_req_duration, p99 = 44.889 ms < 50 ms) is
// used and the gate passes.
func TestOverheadCheck_FallbackMetric_Pass(t *testing.T) {
	data := fixture(t, "k6_summary_fallback_no_custom_metric.json")
	res, err := parseK6SummaryForOverhead(data, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MetricUsed != "http_req_duration" {
		t.Errorf("MetricUsed = %q, want http_req_duration", res.MetricUsed)
	}
	if res.P99MS != 44.889 {
		t.Errorf("P99MS = %v, want 44.889", res.P99MS)
	}
	if !res.Pass {
		t.Errorf("Pass = false, want true (fallback p99=%.3f < threshold=%.0f)", res.P99MS, res.ThresholdMS)
	}
}

// TestOverheadCheck_NoKnownMetric_Fail asserts that a summary containing neither
// gateway_overhead_ms nor http_req_duration returns an error.
func TestOverheadCheck_NoKnownMetric_Fail(t *testing.T) {
	json := `{
		"metrics": {
			"vus": {"type":"gauge","values":{"value":0,"min":0,"max":50}},
			"iterations": {"type":"counter","values":{"count":100,"rate":10}}
		}
	}`
	_, err := parseK6SummaryForOverhead([]byte(json), 50)
	if err == nil {
		t.Fatal("expected error when no known metric is present, got nil")
	}
}

// TestOverheadCheck_MetricMissingP99_Fail asserts that a metric present without
// the p(99) key returns an error rather than silently returning 0.
func TestOverheadCheck_MetricMissingP99_Fail(t *testing.T) {
	json := `{
		"metrics": {
			"gateway_overhead_ms": {
				"type": "trend",
				"values": {"avg": 8.0, "min": 1.0, "max": 40.0}
			}
		}
	}`
	_, err := parseK6SummaryForOverhead([]byte(json), 50)
	if err == nil {
		t.Fatal("expected error when p(99) key is absent, got nil")
	}
}

// TestOverheadCheck_CustomThreshold asserts that the threshold comparison uses
// the supplied value, not the default 50 ms.
func TestOverheadCheck_CustomThreshold(t *testing.T) {
	// p99 = 38.475 ms (under-threshold fixture)
	data := fixture(t, "k6_summary_under_threshold.json")

	// With threshold = 40 ms: 38.475 < 40 → pass.
	res, err := parseK6SummaryForOverhead(data, 40)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Pass {
		t.Errorf("Pass = false with threshold 40, want true (p99=%.3f)", res.P99MS)
	}

	// With threshold = 30 ms: 38.475 >= 30 → fail.
	res2, err := parseK6SummaryForOverhead(data, 30)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.Pass {
		t.Errorf("Pass = true with threshold 30, want false (p99=%.3f)", res2.P99MS)
	}
}

// TestOverheadCheck_ResultFields asserts ThresholdMS is propagated faithfully.
func TestOverheadCheck_ResultFields(t *testing.T) {
	data := fixture(t, "k6_summary_under_threshold.json")
	const threshold = 75.0
	res, err := parseK6SummaryForOverhead(data, threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ThresholdMS != threshold {
		t.Errorf("ThresholdMS = %v, want %v", res.ThresholdMS, threshold)
	}
}

// TestOverheadCheck_K6Args pins the exact k6 invocation shape: summary export to
// the given path, BASE_URL passed via -e (the API key is NOT in argv), script
// last. A drift here silently changes what the BFF.9d validator measures.
func TestOverheadCheck_K6Args(t *testing.T) {
	got := k6Args(k6RunParams{
		Script: "testdata/overhead_loadtest.js", BaseURL: "http://main:8080", SummaryPath: "/tmp/s.json",
	})
	want := []string{
		"run",
		"--no-thresholds",
		"--summary-trend-stats", "avg,min,med,max,p(90),p(95),p(99)",
		"--summary-export", "/tmp/s.json",
		"-e", "BASE_URL=http://main:8080",
		"testdata/overhead_loadtest.js",
	}
	if len(got) != len(want) {
		t.Fatalf("k6Args len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("k6Args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// fakeK6 writes a stub executable that emulates `k6 run --summary-export <path>`:
// it locates the --summary-export argument and writes the given JSON there,
// exiting with the given code. Returns the stub's path.
func fakeK6(t *testing.T, summaryJSON string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "k6")
	script := `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--summary-export" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then printf '%s' '` + summaryJSON + `' > "$out"; fi
exit ` + fmt.Sprintf("%d", exitCode) + `
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake k6: %v", err)
	}
	return stub
}

// TestOverheadCheck_RunK6Summary_Success asserts that a zero-exit k6 stub's
// summary export is read back verbatim.
func TestOverheadCheck_RunK6Summary_Success(t *testing.T) {
	const summary = `{"metrics":{"gateway_overhead_ms":{"type":"trend","values":{"p(99)":12.5}}}}`
	stub := fakeK6(t, summary, 0)

	var stdout, stderr bytes.Buffer
	data, err := runK6Summary(stub, k6RunParams{Script: "testdata/overhead_loadtest.js", BaseURL: "http://main:8080"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != summary {
		t.Errorf("summary bytes = %q, want %q", data, summary)
	}

	res, err := parseK6SummaryForOverhead(data, 50)
	if err != nil {
		t.Fatalf("parse of k6-run summary: %v", err)
	}
	if !res.Pass || res.P99MS != 12.5 {
		t.Errorf("res = %+v, want Pass=true P99MS=12.5", res)
	}
}

// TestOverheadCheck_RunK6Summary_NonZeroExit asserts a failing k6 run surfaces
// an error (the gate must not read a partial summary from a failed run).
func TestOverheadCheck_RunK6Summary_NonZeroExit(t *testing.T) {
	stub := fakeK6(t, `{}`, 3)
	_, err := runK6Summary(stub, k6RunParams{Script: "script.js", BaseURL: "http://main:8080"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error for non-zero k6 exit, got nil")
	}
}

// TestOverheadCheck_RunK6Summary_MissingBinary asserts a clear error when the
// k6 binary does not exist.
func TestOverheadCheck_RunK6Summary_MissingBinary(t *testing.T) {
	_, err := runK6Summary(filepath.Join(t.TempDir(), "no-such-k6"), k6RunParams{Script: "script.js", BaseURL: "http://main:8080"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing k6 binary, got nil")
	}
}

// TestOverheadCheck_RunK6Summary_NoExport asserts an error when k6 exits 0 but
// never writes the summary export (e.g. wrong k6 version flag handling).
func TestOverheadCheck_RunK6Summary_NoExport(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "k6")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	_, err := runK6Summary(stub, k6RunParams{Script: "script.js", BaseURL: "http://main:8080"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error when summary export is missing, got nil")
	}
}

// TestOverheadCheck_WriteBenchmarkFile_RoundTrip asserts the persisted record
// (design §10.2: testdata/p99_overhead_benchmark.json) round-trips faithfully
// and creates missing parent directories.
func TestOverheadCheck_WriteBenchmarkFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "p99_overhead_benchmark.json")
	want := benchmarkRecord{
		Gate:        "BFF.9d",
		MetricUsed:  "gateway_overhead_ms",
		P99MS:       38.475,
		ThresholdMS: 50,
		Pass:        true,
		Source:      "k6-run",
		Script:      "testdata/overhead_loadtest.js",
		BaseURL:     "http://localhost:8080",
		GeneratedAt: "2026-07-25T00:00:00Z",
	}
	if err := writeBenchmarkFile(path, want); err != nil {
		t.Fatalf("writeBenchmarkFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got benchmarkRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
	if data[len(data)-1] != '\n' {
		t.Error("benchmark file must end with a trailing newline")
	}
}

// TestOverheadCheck_WriteBenchmarkFile_BadDir asserts a write into an
// uncreatable directory (a path component that is a file) errors cleanly.
func TestOverheadCheck_WriteBenchmarkFile_BadDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	err := writeBenchmarkFile(filepath.Join(blocker, "bench.json"), benchmarkRecord{Gate: "BFF.9d"})
	if err == nil {
		t.Fatal("expected error when parent dir is a file, got nil")
	}
}

// TestOverheadCheck_ResolveBenchmarkOut pins the persistence policy: live k6
// runs persist to the default path under "auto"; hermetic (--summary) runs do
// NOT persist under "auto" (a fixture-driven CI invocation must never overwrite
// the operator's real benchmark); explicit paths always win; "" always disables.
func TestOverheadCheck_ResolveBenchmarkOut(t *testing.T) {
	cases := []struct {
		flagVal, source, want string
	}{
		{"auto", "k6-run", defaultBenchmarkPath},
		{"auto", "summary-file", ""},
		{"", "k6-run", ""},
		{"", "summary-file", ""},
		{"custom/out.json", "k6-run", "custom/out.json"},
		{"custom/out.json", "summary-file", "custom/out.json"},
	}
	for _, c := range cases {
		if got := resolveBenchmarkOut(c.flagVal, c.source); got != c.want {
			t.Errorf("resolveBenchmarkOut(%q, %q) = %q, want %q", c.flagVal, c.source, got, c.want)
		}
	}
}

// TestOverheadCheck_K6Args_ProjectsAndModel pins the extended invocation: MODEL
// and the PROJECTS selector list are appended before the script path.
func TestOverheadCheck_K6Args_ProjectsAndModel(t *testing.T) {
	got := k6Args(k6RunParams{
		Script: "s.js", BaseURL: "http://main:8080", SummaryPath: "/tmp/s.json",
		Model:    "openai/Qwen/Qwen3.6-35B-A3B-FP8",
		Projects: []string{"9103", "9104"},
	})
	want := []string{
		"run",
		"--no-thresholds",
		"--summary-trend-stats", "avg,min,med,max,p(90),p(95),p(99)",
		"--summary-export", "/tmp/s.json",
		"-e", "BASE_URL=http://main:8080",
		"-e", "MODEL=openai/Qwen/Qwen3.6-35B-A3B-FP8",
		"-e", "PROJECTS=9103,9104",
		"s.js",
	}
	if len(got) != len(want) {
		t.Fatalf("k6Args len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("k6Args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// k6V2Summary renders a k6 v2 --summary-export document in the FLAT shape the
// live BFF.9d run produces (stats directly on the metric object, no "values"
// nesting). failedRate is the http_req_failed rate, i.e. the fraction of
// requests that FAILED. A negative fallbackCount omits the
// gateway_overhead_fallback counter entirely, as a run with no fallback samples
// does; a negative overheadP99 omits the custom trend.
func k6V2Summary(reqs, iters int, failedRate, overheadP99 float64, fallbackCount int) string {
	fails := int(float64(reqs) * failedRate)
	metrics := fmt.Sprintf(`"http_reqs":{"count":%d,"rate":50.0},
		"iterations":{"count":%d,"rate":50.0},
		"http_req_failed":{"rate":%g,"passes":%d,"fails":%d},
		"http_req_duration":{"avg":142.5,"min":89.0,"med":138.2,"max":521.3,"p(90)":201.4,"p(95)":263.8,"p(99)":412.0}`,
		reqs, iters, failedRate, fails, reqs-fails)
	if overheadP99 >= 0 {
		metrics += fmt.Sprintf(`,
		"gateway_overhead_ms":{"avg":8.09,"min":1.26,"med":6.88,"max":29.17,"p(90)":14.07,"p(95)":17.47,"p(99)":%g}`, overheadP99)
	}
	if fallbackCount >= 0 {
		metrics += fmt.Sprintf(`,
		"gateway_overhead_fallback":{"count":%d,"rate":1.0}`, fallbackCount)
	}
	return "{\"metrics\":{" + metrics + "}}"
}

// TestOverheadCheck_AllRequestsFailed_Fail is the regression test for the
// faked-validator failure mode: k6 runs with --no-thresholds, so a run in which
// every request errored still exports a summary with a small, meaningless p99.
// The gate MUST reject it on run health before it ever looks at the percentile.
func TestOverheadCheck_AllRequestsFailed_Fail(t *testing.T) {
	// 3 000 requests, 100% failed, p99 of 4 ms — well "under" the 50 ms bar.
	data := k6V2Summary(3000, 3000, 1.0, 4.2, 0)

	// The latency half alone happily reports a pass — that is the bug.
	if res, err := parseK6SummaryForOverhead([]byte(data), 50); err != nil || !res.Pass {
		t.Fatalf("precondition: latency-only parse should report a pass (res=%+v err=%v)", res, err)
	}
	// The gate must not.
	if _, err := evaluateK6Summary([]byte(data), 50, false); err == nil {
		t.Fatal("evaluateK6Summary passed an all-errors run — the gate is faked")
	}
}

// TestOverheadCheck_SuccessRateBoundary pins the >= 99% success requirement:
// 98.5% fails, 99.5% passes.
func TestOverheadCheck_SuccessRateBoundary(t *testing.T) {
	if _, err := evaluateK6Summary([]byte(k6V2Summary(3000, 3000, 0.015, 20.6, 0)), 50, false); err == nil {
		t.Error("98.5 percent success rate must fail the gate")
	}
	if _, err := evaluateK6Summary([]byte(k6V2Summary(3000, 3000, 0.005, 20.6, 0)), 50, false); err != nil {
		t.Errorf("99.5%% success rate must pass the gate: %v", err)
	}
}

// TestOverheadCheck_ZeroWork_Fail asserts a run that completed no iterations or
// issued no requests is rejected — there is nothing to measure.
func TestOverheadCheck_ZeroWork_Fail(t *testing.T) {
	cases := map[string]string{
		"zero iterations": k6V2Summary(3000, 0, 0, 20.6, 0),
		"zero requests":   k6V2Summary(0, 3000, 0, 20.6, 0),
		"zero both":       k6V2Summary(0, 0, 0, 20.6, 0),
	}
	for name, data := range cases {
		if _, err := evaluateK6Summary([]byte(data), 50, false); err == nil {
			t.Errorf("%s: expected gate failure, got pass", name)
		}
	}
}

// TestOverheadCheck_HealthySummary_Pass asserts a healthy run in the real k6 v2
// flat shape still passes, with the health view populated for the audit record.
func TestOverheadCheck_HealthySummary_Pass(t *testing.T) {
	res, err := evaluateK6Summary([]byte(k6V2Summary(3000, 3000, 0.0, 20.62, 0)), 50, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Pass || res.MetricUsed != "gateway_overhead_ms" || res.P99MS != 20.62 {
		t.Errorf("res = %+v, want pass on gateway_overhead_ms p99=20.62", res)
	}
	if res.Health.Requests != 3000 || res.Health.SuccessRate() != 1 {
		t.Errorf("health = %+v, want 3000 requests at 100%% success", res.Health)
	}
}

// TestOverheadCheck_NoFailedRateMetric_Fail asserts a summary that omits
// http_req_failed cannot pass: the gate cannot prove the run succeeded.
func TestOverheadCheck_NoFailedRateMetric_Fail(t *testing.T) {
	data := `{"metrics":{
		"http_reqs":{"count":3000,"rate":50.0},
		"iterations":{"count":3000,"rate":50.0},
		"gateway_overhead_ms":{"p(99)":20.62}
	}}`
	if _, err := evaluateK6Summary([]byte(data), 50, false); err == nil {
		t.Fatal("expected failure when http_req_failed is absent, got pass")
	}
}

// TestOverheadCheck_RoundTripFallback_LoudByDefault asserts the two silent
// degradations both fail closed, and that --allow-roundtrip-fallback is the
// only way to accept them.
func TestOverheadCheck_RoundTripFallback_LoudByDefault(t *testing.T) {
	// (1) The script had to fill gateway_overhead_ms from the round-trip.
	viaCounter := k6V2Summary(3000, 3000, 0.0, 20.62, 7)
	if _, err := evaluateK6Summary([]byte(viaCounter), 50, false); err == nil {
		t.Error("non-zero gateway_overhead_fallback must fail the strict gate")
	}
	if _, err := evaluateK6Summary([]byte(viaCounter), 50, true); err != nil {
		t.Errorf("--allow-roundtrip-fallback must accept fallback samples: %v", err)
	}

	// (2) The custom metric is missing entirely, leaving only http_req_duration
	// (p99 = 412 ms here, so the opt-in run still fails on the threshold — the
	// point is that strict mode fails for a DIFFERENT, louder reason).
	noCustom := k6V2Summary(3000, 3000, 0.0, -1, -1)
	if _, err := evaluateK6Summary([]byte(noCustom), 50, false); err == nil {
		t.Error("missing gateway_overhead_ms must fail the strict gate")
	}
	res, err := evaluateK6Summary([]byte(noCustom), 500, true)
	if err != nil {
		t.Fatalf("--allow-roundtrip-fallback must accept http_req_duration: %v", err)
	}
	if res.MetricUsed != "http_req_duration" || !res.Pass {
		t.Errorf("res = %+v, want passing http_req_duration fallback", res)
	}
}

// TestOverheadCheck_K6V2FlatSummaryShape pins the k6 v2 --summary-export shape
// (stats directly on the metric object, no "values" nesting) — the format the
// live BFF.9d run actually produces.
func TestOverheadCheck_K6V2FlatSummaryShape(t *testing.T) {
	summary := `{"metrics":{"gateway_overhead_ms":{"p(90)":14.07,"p(95)":17.47,"p(99)":20.62,"avg":8.09,"min":1.26,"med":6.88,"max":29.17}}}`
	res, err := parseK6SummaryForOverhead([]byte(summary), 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MetricUsed != "gateway_overhead_ms" || res.P99MS != 20.62 || !res.Pass {
		t.Errorf("res = %+v, want gateway_overhead_ms p99=20.62 pass", res)
	}
}

// TestOverheadCheck_ProjectsFromFile reads the project ids the load spreads
// across, and refuses what would corrupt the comma-separated PROJECTS env.
func TestOverheadCheck_ProjectsFromFile(t *testing.T) {
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "projects.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got, err := projectsFromFile(write(`[{"project_id":"9101","user_id":"u"},{"project_id":" 9102 "}]`))
	if err != nil {
		t.Fatalf("projectsFromFile: %v", err)
	}
	if len(got) != 2 || got[0] != "9101" || got[1] != "9102" {
		t.Errorf("projects = %v, want [9101 9102]", got)
	}
	for name, body := range map[string]string{
		"empty":     `[]`,
		"no id":     `[{"user_id":"u"}]`,
		"comma":     `[{"project_id":"1,2"}]`,
		"malformed": `{`,
	} {
		if _, err := projectsFromFile(write(body)); err == nil {
			t.Errorf("%s: projectsFromFile accepted %s", name, body)
		}
	}
	if _, err := projectsFromFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("projectsFromFile accepted a missing file")
	}
}
