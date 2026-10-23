package oapiserver_test

// Regression tests for the two matcher false-negative classes demonstrated by
// adversarial verification of unit W1. Both are pure unit tests of the
// collector/matcher against the real router — no v2.yaml involvement — and
// each MUST report the probe as unmatched:
//
//   Probe (d) / F1: the doubled-prefix compat shim
//     r.HandleFunc("/api/v2/api/v2/*", ...) (internal/api/router.go:198) is
//     registered for every method. Before the fix, a bogus spec op
//     `POST /api/v2/zz_bogus` resolved vacuously, because root server
//     `/api/v2` + path produced /api/v2/api/v2/zz_bogus, which the shim
//     swallowed. CollectRoutes now excludes compat shims.
//
//   Probe (e) / F2: chi.Walk reports the nested Mount("/") of the
//     budget-alert routes (internal/api/router.go:284 +
//     internal/api/gateway/budget_alerts.go:93-94) as the pattern
//     /api/v2/admin/gateway/*/budget-alerts. Before the fix the mid-pattern
//     "*" was treated as a tail-swallow, so a bogus `GET
//     /admin/gateway/zz_bogus` matched by discarding "budget-alerts".
//     normalizeSegments now collapses non-trailing "*" (the runtime path has
//     zero segments at that position); only a TRAILING "*" may swallow.

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/oapiserver"
)

func collectFullSurface(t *testing.T) *oapiserver.RouteSet {
	t.Helper()
	routes, err := oapiserver.CollectRoutes(api.NewRouter(buildFullSurfaceConfig()))
	if err != nil {
		t.Fatalf("collecting routes: %v", err)
	}
	return routes
}

// TestMatcherProbeD_CompatShimDoesNotResolveBogusOps encodes verifier probe
// (d): no candidate of a bogus /api/v2-prefixed spec operation may resolve
// via the doubled-prefix rewrite shim.
func TestMatcherProbeD_CompatShimDoesNotResolveBogusOps(t *testing.T) {
	routes := collectFullSurface(t)

	op := oapiserver.SpecOperation{
		OperationID: "zzBogusDoubledPrefix",
		Method:      "POST",
		Path:        "/api/v2/zz_bogus",
		// Same base paths the real spec's root servers produce.
		BasePaths: []string{"/api/v2/elitea_core", "/api/v2"},
	}
	for _, cand := range op.CandidatePaths() {
		if routes.Resolves(op.Method, cand) {
			t.Errorf("bogus operation %s resolved via candidate %q — the compat shim (router.go:198) is masking drift again", op.OperationID, cand)
		}
	}

	// The same must hold for the other all-method shims.
	for _, probe := range []struct{ method, path string }{
		{"GET", "/llm/zz_bogus"},
		{"GET", "/app/application_icon/zz_bogus.png"},
		{"GET", "/app/application_tool_icon/zz_bogus.png"},
	} {
		if routes.Resolves(probe.method, probe.path) {
			t.Errorf("bogus path %s %s resolved via a passthrough shim that CollectRoutes should exclude", probe.method, probe.path)
		}
	}
}

// TestMatcherProbeE_MidPatternWildcardDoesNotSwallow encodes verifier probe
// (e): the walked mount artifact /api/v2/admin/gateway/*/budget-alerts must
// not let arbitrary /admin/gateway/... paths resolve, while the real
// runtime shape (zero segments at the "*") must still resolve.
func TestMatcherProbeE_MidPatternWildcardDoesNotSwallow(t *testing.T) {
	routes := collectFullSurface(t)

	// Negative: the pre-fix false negative — "*" swallowing "budget-alerts".
	for _, probe := range []struct{ method, path string }{
		{"GET", "/api/v2/admin/gateway/zz_bogus"},
		{"PUT", "/api/v2/admin/gateway/zz_bogus"},
		// "exactly one extra segment" shapes must not match either: the
		// runtime path has NOTHING between gateway/ and budget-alerts.
		{"GET", "/api/v2/admin/gateway/zz_bogus/budget-alerts"},
	} {
		if routes.Resolves(probe.method, probe.path) {
			t.Errorf("bogus path %s %s resolved — mid-pattern \"*\" is swallowing segments again", probe.method, probe.path)
		}
	}

	// Positive control: the collapsed pattern must keep matching the shape
	// the router actually serves (Mount(\"/\") => zero segments at \"*\").
	for _, probe := range []struct{ method, path string }{
		{"GET", "/api/v2/admin/gateway/budget-alerts"},
		{"PUT", "/api/v2/admin/gateway/budget-alerts"},
	} {
		if !routes.Resolves(probe.method, probe.path) {
			t.Errorf("legitimate path %s %s no longer resolves — the mid-pattern collapse is over-aggressive", probe.method, probe.path)
		}
	}

	// Positive control: a genuine TRAILING wildcard still swallows a
	// multi-segment remainder (S3 object keys contain slashes).
	if !routes.Resolves("GET", "/api/v2/artifacts/objects/{projectID}/{bucket}/deep/object/key") {
		t.Error("trailing-wildcard route /api/v2/artifacts/objects/{projectID}/{bucket}/* no longer swallows a multi-segment object key")
	}
}
