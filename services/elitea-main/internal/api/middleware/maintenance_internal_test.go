package middleware

// The maintenance gate's decisions, without a database.
//
// Every test here covers a choice the file doc argues for, because those are the
// ones a later change can quietly reverse: which paths escape the window, which
// failure direction each dependency takes, and what a failed refresh does to a
// window that is already running.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type stubResolver struct {
	permissions []string
	err         error
}

func (s stubResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	if s.err != nil {
		return auth.PermissionResolution{}, s.err
	}
	return auth.PermissionResolution{Permissions: s.permissions}, nil
}

// onWindow is a gate already inside a maintenance window.
func onWindow(resolver auth.PermissionResolver) *maintenanceGate {
	return &maintenanceGate{
		load: func(context.Context) (platformconfig.Maintenance, error) {
			return platformconfig.Maintenance{
				Enabled: true,
				Title:   "Down for maintenance",
				Message: "Back at 14:00 UTC.",
			}, nil
		},
		resolver: resolver,
	}
}

// serve runs one request through the gate and reports the status and whether the
// wrapped handler ran.
func serve(gate *maintenanceGate, path string, user *auth.User) (int, bool) {
	reached := false
	handler := gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, reached
}

// TestMaintenanceRefusesANonAdministrator is the feature: an authenticated user
// who is not an administrator is refused, with the operator's own words and the
// `maintenance` discriminator that tells a client this 503 is deliberate rather
// than an overloaded gateway.
func TestMaintenanceRefusesANonAdministrator(t *testing.T) {
	gate := onWindow(stubResolver{permissions: []string{"projects.projects.projects.view"}})
	user := auth.User{ID: "7"}

	recorder := httptest.NewRecorder()
	handler := gate.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran during a maintenance window")
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/agents", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), user))
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Error("no Retry-After header on a deliberate, temporary refusal")
	}
	body := recorder.Body.String()
	for _, want := range []string{`"maintenance":true`, "Down for maintenance", "Back at 14:00 UTC."} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s omits %q", body, want)
		}
	}
}

// TestMaintenanceAdmitsAnAdministrator — the switch must stay reversible from
// inside the window it created.
func TestMaintenanceAdmitsAnAdministrator(t *testing.T) {
	gate := onWindow(stubResolver{permissions: []string{MaintenanceAdminPermission}})
	user := auth.User{ID: "1"}

	status, reached := serve(gate, "/api/v2/elitea_core/agents", &user)
	if status != http.StatusOK || !reached {
		t.Fatalf("status = %d, reached = %v; want 200 and the handler reached", status, reached)
	}
}

// TestMaintenanceRefusesAnonymousAndUnresolvableCallers.
//
// Both are refused, and both are the LESS permissive answer — the opposite of
// the switch read itself. Admitting a caller whose permissions could not be
// resolved would open a platform an operator deliberately closed.
func TestMaintenanceRefusesAnonymousAndUnresolvableCallers(t *testing.T) {
	anonymous := onWindow(stubResolver{permissions: []string{MaintenanceAdminPermission}})
	if status, _ := serve(anonymous, "/api/v2/elitea_core/agents", nil); status != http.StatusServiceUnavailable {
		t.Errorf("anonymous status = %d, want 503", status)
	}

	broken := onWindow(stubResolver{err: errors.New("pool exhausted")})
	user := auth.User{ID: "1"}
	if status, _ := serve(broken, "/api/v2/elitea_core/agents", &user); status != http.StatusServiceUnavailable {
		t.Errorf("unresolvable status = %d, want 503", status)
	}
}

// TestMaintenanceAllowlistEscapesTheWindow — the four prefixes that must keep
// working, checked through the middleware rather than against the helper, so a
// change that moves the check out of the request path still fails this.
func TestMaintenanceAllowlistEscapesTheWindow(t *testing.T) {
	gate := onWindow(stubResolver{permissions: nil})
	for _, path := range []string{
		"/api/v2/auth/login",
		"/api/v2/scim/v2/Users",
		"/api/v2/admin/plugin_config_values/administration/maintenance",
		"/api/v2/elitea_core/platform_settings/prompt_lib",
	} {
		status, reached := serve(gate, path, nil)
		if status != http.StatusOK || !reached {
			t.Errorf("%s: status = %d, reached = %v; want the allowlist to escape the window",
				path, status, reached)
		}
	}
}

// TestMaintenanceAllowlistMatchesOnSegmentBoundaries is the hole a plain
// HasPrefix would leave: a route whose name merely STARTS with an allowlisted
// prefix must not escape the window.
func TestMaintenanceAllowlistMatchesOnSegmentBoundaries(t *testing.T) {
	if maintenanceExempt("/api/v2/administration_of_secrets") {
		t.Error(`"/api/v2/administration_of_secrets" escaped the window on a "/api/v2/admin" prefix`)
	}
	if maintenanceExempt("/api/v2/authors") {
		t.Error(`"/api/v2/authors" escaped the window on a "/api/v2/auth" prefix`)
	}
	if !maintenanceExempt("/api/v2/admin") {
		t.Error("the bare allowlist prefix itself was not exempt")
	}
}

// TestMaintenanceCacheHoldsTheSwitchForItsTTL — one read per TTL, not one per
// request. This is the whole reason the cache exists.
func TestMaintenanceCacheHoldsTheSwitchForItsTTL(t *testing.T) {
	reads := 0
	clock := time.Unix(1_700_000_000, 0)
	gate := &maintenanceGate{
		load: func(context.Context) (platformconfig.Maintenance, error) {
			reads++
			return platformconfig.Maintenance{Enabled: false}, nil
		},
		resolver: stubResolver{},
		now:      func() time.Time { return clock },
	}

	for range 5 {
		serve(gate, "/api/v2/elitea_core/agents", nil)
	}
	if reads != 1 {
		t.Fatalf("reads = %d within one TTL, want 1", reads)
	}

	clock = clock.Add(maintenanceCacheTTL + time.Second)
	serve(gate, "/api/v2/elitea_core/agents", nil)
	if reads != 2 {
		t.Fatalf("reads = %d after the TTL expired, want 2", reads)
	}
}

// TestAFailedRefreshDoesNotEndARunningWindow.
//
// The refresh failure direction differs from the COLD-start one on purpose: a
// window that is already running must not be lifted by a query timeout, while a
// gate that has never successfully read anything defaults to "the platform is
// up". Both are stated in the file doc; this pins them.
func TestAFailedRefreshDoesNotEndARunningWindow(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	failing := false
	gate := &maintenanceGate{
		load: func(context.Context) (platformconfig.Maintenance, error) {
			if failing {
				return platformconfig.Maintenance{}, errors.New("query timeout")
			}
			return platformconfig.Maintenance{Enabled: true, Title: "t", Message: "m"}, nil
		},
		resolver: stubResolver{},
		now:      func() time.Time { return clock },
	}

	if status, _ := serve(gate, "/api/v2/elitea_core/agents", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d before the failure, want 503", status)
	}

	failing = true
	clock = clock.Add(maintenanceCacheTTL + time.Second)
	if status, _ := serve(gate, "/api/v2/elitea_core/agents", nil); status != http.StatusServiceUnavailable {
		t.Errorf("status = %d after a failed refresh, want the running window to survive it", status)
	}
}

// TestAColdGateThatCannotReadTheStoreLetsTrafficThrough — the permissive
// default, which is the direction that cannot cause an outage nobody asked for.
func TestAColdGateThatCannotReadTheStoreLetsTrafficThrough(t *testing.T) {
	gate := &maintenanceGate{
		load: func(context.Context) (platformconfig.Maintenance, error) {
			return platformconfig.Maintenance{}, errors.New("connection refused")
		},
		resolver: stubResolver{},
	}
	status, reached := serve(gate, "/api/v2/elitea_core/agents", nil)
	if status != http.StatusOK || !reached {
		t.Fatalf("status = %d, reached = %v; want an unreadable store to mean 'not in maintenance'",
			status, reached)
	}
}

// TestMaintenanceIsAPassThroughWithoutItsDependencies — a half-wired deployment
// must not half-enforce. A nil resolver in particular would refuse EVERYONE,
// including the administrator who would have to end the window.
func TestMaintenanceIsAPassThroughWithoutItsDependencies(t *testing.T) {
	for name, config := range map[string]MaintenanceConfig{
		"no pool":     {Resolver: stubResolver{}},
		"no resolver": {},
	} {
		reached := false
		handler := Maintenance(config)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			reached = true
		}))
		handler.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/agents", nil))
		if !reached {
			t.Errorf("%s: the middleware was not a pass-through", name)
		}
	}
}
