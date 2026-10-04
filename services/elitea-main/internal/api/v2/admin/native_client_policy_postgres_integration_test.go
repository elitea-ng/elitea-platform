package admin_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/nativepolicy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type invalidationSpy struct{ calls int }

func (s *invalidationSpy) Invalidate() { s.calls++ }

const nativePolicyPath = "/admin/plugin_config_values/administration/native_client_policy"

// TestNativeClientPolicySaveReachesTheReader — a valid save lands, drops the
// shared cache, and the reader the three consumers use returns it.
func TestNativeClientPolicySaveReachesTheReader(t *testing.T) {
	pool := newConfigPool(t)
	spy := &invalidationSpy{}
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := configRouter(admin.NewHandler(pool,
		admin.WithPermissionResolver(grantingResolver("configuration.native_clients")),
		admin.WithNativeClientPolicy(spy),
	), &principal)

	// A fresh install reads the defaults (live: no 501).
	recorder := configDo(t, router, http.MethodGet, nativePolicyPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := decodeConfigBody(t, recorder).Values["offline_retention_days"]; got != float64(30) {
		t.Fatalf("default offline_retention_days = %v, want 30", got)
	}

	policy := nativepolicy.New(pool, nil)
	before, err := policy.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.MinClientVersion != "" {
		t.Fatalf("fresh minimum = %q", before.MinClientVersion)
	}

	recorder = configDo(t, router, http.MethodPut, nativePolicyPath, map[string]any{"values": map[string]any{
		"require_device_lock": true, "idle_lock_seconds": 120, "offline_retention_days": 0,
		"min_client_version": "2.1.0",
	}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if spy.calls != 1 {
		t.Fatalf("the save invalidated the policy cache %d times, want 1", spy.calls)
	}
	if raw, ok := storedValueSQL(t, pool, "native_client_policy", "min_client_version"); !ok || raw != `"2.1.0"` {
		t.Fatalf("stored min_client_version = %q, %v", raw, ok)
	}

	policy.Invalidate() // what the spy stands in for
	after, err := policy.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !after.RequireDeviceLock || after.IdleLockSeconds != 120 || after.OfflineEnabled() || after.MinClientVersion != "2.1.0" {
		t.Fatalf("reader after save = %+v", after)
	}
}

// TestNativeClientPolicyRefusesOutOfRangeAndStoresNothing — the section hook
// refuses before the all-or-nothing write, so a partly valid body leaves no row.
func TestNativeClientPolicyRefusesOutOfRangeAndStoresNothing(t *testing.T) {
	pool := newConfigPool(t)
	spy := &invalidationSpy{}
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := configRouter(admin.NewHandler(pool,
		admin.WithPermissionResolver(grantingResolver("configuration.native_clients")),
		admin.WithNativeClientPolicy(spy),
	), &principal)

	for name, values := range map[string]map[string]any{
		"retention above the cap": {"require_device_lock": true, "offline_retention_days": 365},
		"fractional seconds":      {"require_device_lock": true, "idle_lock_seconds": 2.5},
		"bad version":             {"require_device_lock": true, "min_client_version": "v-next"},
		"wrong type":              {"require_device_lock": true, "offline_max_mb": "512"},
	} {
		recorder := configDo(t, router, http.MethodPut, nativePolicyPath, map[string]any{"values": values})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", name, recorder.Code, recorder.Body.String())
		}
	}
	if _, stored := storedValueSQL(t, pool, "native_client_policy", "require_device_lock"); stored {
		t.Fatal("a refused save stored its valid fields")
	}
	if spy.calls != 0 {
		t.Fatalf("a refused save invalidated the cache %d times", spy.calls)
	}
}

// TestNativeClientPolicyRequiresTheNativeClientsPermission — decision 6: the
// one `configuration.native_clients` grant guards it; runtime.plugins does not.
func TestNativeClientPolicyRequiresTheNativeClientsPermission(t *testing.T) {
	pool := newConfigPool(t)
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := configRouter(admin.NewHandler(pool,
		admin.WithPermissionResolver(grantingResolver("runtime.plugins")),
	), &principal)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"values": map[string]any{"require_device_lock": true}}
		}
		recorder := configDo(t, router, method, nativePolicyPath, body)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403 (body %s)", method, recorder.Code, recorder.Body.String())
		}
	}
}
