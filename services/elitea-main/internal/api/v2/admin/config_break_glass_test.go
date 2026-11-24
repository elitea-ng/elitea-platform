package admin_test

// The break_glass Configuration section (ADR-0059 decision 5, owner
// decisions 2 and 5; story R2.2a, issue 2516) through the product's own
// routes, on the shared corpus so the security audit outbox is real:
//
//   - a cap above its ceiling in code (481 minutes online, 241 sealed) is a
//     400, and nothing is stored;
//   - switching approval off is refused (403) to a holder of the section
//     without a STANDING admin.auth.users.super_admin, an elevated one
//     included; a super_admin gets 200 and exactly one admin.access.changed
//     event, in the save's transaction: with the appender failing, nothing
//     is stored;
//   - switching it on again takes effect at once and writes its own event;
//   - a caller without access.breakglass.manage gets 403 on the values and
//     does not see the section in the schema list.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const breakGlassValuesPath = "/admin/plugin_config_values/administration/break_glass"

var breakGlassTestClock = time.Date(2026, time.November, 24, 9, 0, 0, 0, time.UTC)

// breakGlassResolver answers a standing set and, optionally, permissions only
// an elevation gives.
func breakGlassResolver(elevated []string, standing ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{
			UserID:              7,
			Permissions:         append(append([]string{}, standing...), elevated...),
			ElevatedPermissions: elevated,
		}, nil
	}
}

func breakGlassRouter(t *testing.T, pool *pgxpool.Pool, resolver auth.PermissionResolver, options ...admin.Option) chi.Router {
	t.Helper()
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	options = append([]admin.Option{
		admin.WithPermissionResolver(resolver),
		admin.WithClock(func() time.Time { return breakGlassTestClock }),
	}, options...)
	return configRouter(admin.NewHandler(pool, options...), &principal)
}

func saveBreakGlass(t *testing.T, router chi.Router, values map[string]any) (int, configValuesBody) {
	t.Helper()
	recorder := configDo(t, router, http.MethodPut, breakGlassValuesPath, map[string]any{"values": values})
	var body configValuesBody
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

// breakGlassApprovalEvents answers the reason codes of the
// admin.access.break_glass_approval events of the outbox, in order.
func breakGlassApprovalEvents(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event -> 'data' -> 'access' ->> 'reason_code'
FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event -> 'data' ->> 'action' = 'admin.access.break_glass_approval'
ORDER BY seq`, string(securityaudit.TypeAdminAccessChanged))
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatal(err)
		}
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return codes
}

func storedBreakGlass(t *testing.T, pool *pgxpool.Pool) platformconfig.Values {
	t.Helper()
	values, err := platformconfig.Load(t.Context(), pool, platformconfig.SectionBreakGlass)
	if err != nil {
		t.Fatalf("read the section: %v", err)
	}
	return values
}

// A cap above the ceiling in code is a 400 and stores nothing; at the ceiling
// it is saved.
func TestBreakGlassCapsStopAtTheCeilings(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	router := breakGlassRouter(t, pool, breakGlassResolver(nil, adminaccess.AccessBreakglassManage))

	for name, values := range map[string]map[string]any{
		"online 481":                {platformconfig.KeyBreakGlassOnlineMaxMinutes: 481},
		"sealed 241":                {platformconfig.KeyBreakGlassSealedMaxMinutes: 241},
		"a default above its cap":   {platformconfig.KeyBreakGlassOnlineDefaultMinutes: 300},
		"a fractional minute":       {platformconfig.KeyBreakGlassRequestLifetimeMins: 1.5},
		"re-authentication raised":  {platformconfig.KeyBreakGlassReauthenticationMins: 16},
		"the approval delay zeroed": {platformconfig.KeyBreakGlassApprovalOffDelayHours: 0},
	} {
		if code, body := saveBreakGlass(t, router, values); code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%s), want 400", name, code, body.Error)
		}
	}
	if stored := storedBreakGlass(t, pool); len(stored) != 0 {
		t.Fatalf("a refused save stored %v", stored)
	}

	code, body := saveBreakGlass(t, router, map[string]any{
		platformconfig.KeyBreakGlassOnlineMaxMinutes: 480,
		platformconfig.KeyBreakGlassSealedMaxMinutes: 240,
	})
	if code != http.StatusOK || body.Values[platformconfig.KeyBreakGlassOnlineMaxMinutes] != float64(480) {
		t.Fatalf("a save at the ceilings: %d %+v", code, body)
	}
	settings := platformconfig.BreakGlassSettingsFromValues(storedBreakGlass(t, pool))
	if settings.OnlineMax != 480*time.Minute || settings.SealedMax != 240*time.Minute {
		t.Fatalf("the reader answers %+v", settings)
	}
}

// Switching approval off needs a standing super_admin; the super_admin's
// switch-off is one event in the save's transaction and takes effect after
// the delay; on again is at once.
func TestBreakGlassApprovalOffNeedsAStandingSuperAdmin(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	off := map[string]any{platformconfig.KeyBreakGlassApprovalRequired: false}

	for name, resolver := range map[string]permissionResolverFunc{
		"admin": breakGlassResolver(nil, adminaccess.AccessBreakglassManage, adminaccess.AdminAuthUsers),
		"super_admin only by an elevation": breakGlassResolver([]string{adminaccess.SuperAdminPermission},
			adminaccess.AccessBreakglassManage),
	} {
		code, _ := saveBreakGlass(t, breakGlassRouter(t, pool, resolver), off)
		if code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, code)
		}
	}
	// The same holder still edits the durations: the rule is on the action.
	admins := breakGlassRouter(t, pool, breakGlassResolver(nil, adminaccess.AccessBreakglassManage))
	if code, body := saveBreakGlass(t, admins, map[string]any{
		platformconfig.KeyBreakGlassApprovalRequired: true,
		platformconfig.KeyBreakGlassOnlineMaxMinutes: 120,
	}); code != http.StatusOK {
		t.Fatalf("a duration edit by a holder without super_admin: %d %s", code, body.Error)
	}
	if events := breakGlassApprovalEvents(t, pool); len(events) != 0 {
		t.Fatalf("events before any switch: %v", events)
	}
	// Shortening the approval-off delay weakens the next switch-off, so it
	// needs the same standing super_admin; lengthening it does not.
	if code, _ := saveBreakGlass(t, admins, map[string]any{platformconfig.KeyBreakGlassApprovalOffDelayHours: 1}); code != http.StatusForbidden {
		t.Fatalf("a shorter delay by a holder without super_admin answered %d, want 403", code)
	}
	if code, body := saveBreakGlass(t, admins, map[string]any{platformconfig.KeyBreakGlassApprovalOffDelayHours: 48}); code != http.StatusOK {
		t.Fatalf("a longer delay by a holder without super_admin: %d %s", code, body.Error)
	}
	if code, body := saveBreakGlass(t, breakGlassRouter(t, pool, breakGlassResolver(nil, adminaccess.AccessBreakglassManage,
		adminaccess.SuperAdminPermission)), map[string]any{platformconfig.KeyBreakGlassApprovalOffDelayHours: 24}); code != http.StatusOK {
		t.Fatalf("a shorter delay by a standing super_admin: %d %s", code, body.Error)
	}

	superAdmin := breakGlassResolver(nil, adminaccess.AccessBreakglassManage, adminaccess.SuperAdminPermission)

	// With the appender failing the switch-off does not happen.
	failing := breakGlassRouter(t, pool, superAdmin, admin.WithAccessAuditAppender(securityaudit.FailingAppender{}))
	if code, _ := saveBreakGlass(t, failing, off); code != http.StatusInternalServerError {
		t.Fatalf("a switch-off whose event fails answered %d, want 500", code)
	}
	if stored := storedBreakGlass(t, pool); stored.Bool(platformconfig.KeyBreakGlassApprovalRequired, true) == false {
		t.Fatalf("a switch-off without its event was stored: %v", stored)
	}

	router := breakGlassRouter(t, pool, superAdmin)
	code, body := saveBreakGlass(t, router, off)
	if code != http.StatusOK {
		t.Fatalf("a super_admin switch-off: %d %s", code, body.Error)
	}
	if events := breakGlassApprovalEvents(t, pool); !slices.Equal(events, []string{platformconfig.BreakGlassApprovalOffRequested}) {
		t.Fatalf("events = %v, want one approval_off_requested", events)
	}
	settings := platformconfig.BreakGlassSettingsFromValues(storedBreakGlass(t, pool))
	if !settings.ApprovalOffEffectiveFrom.Equal(breakGlassTestClock.Add(24 * time.Hour)) {
		t.Fatalf("effective from %s, want the save plus 24 hours", settings.ApprovalOffEffectiveFrom)
	}
	if !settings.At(breakGlassTestClock.Add(time.Hour)).ApprovalRequired ||
		settings.At(breakGlassTestClock.Add(24*time.Hour)).ApprovalRequired {
		t.Fatal("the switch-off is not delayed by 24 hours")
	}
	get := configDo(t, router, http.MethodGet, breakGlassValuesPath, nil)
	var read struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &read); err != nil || read.Status["approval_state"] != "off_scheduled" {
		t.Fatalf("the section status = %v (%v), want off_scheduled", read.Status, err)
	}

	// Saving off again keeps the time and writes nothing.
	if code, _ := saveBreakGlass(t, router, off); code != http.StatusOK {
		t.Fatalf("a second switch-off: %d", code)
	}
	if events := breakGlassApprovalEvents(t, pool); len(events) != 1 {
		t.Fatalf("a second switch-off wrote an event: %v", events)
	}

	// On again: at once, by a holder of the section, with its event.
	if code, _ := saveBreakGlass(t, admins, map[string]any{platformconfig.KeyBreakGlassApprovalRequired: true}); code != http.StatusOK {
		t.Fatalf("switching approval on: %d", code)
	}
	settings = platformconfig.BreakGlassSettingsFromValues(storedBreakGlass(t, pool))
	if !settings.At(breakGlassTestClock.Add(48*time.Hour)).ApprovalRequired || !settings.ApprovalOffEffectiveFrom.IsZero() {
		t.Fatalf("after switching on: %+v", settings)
	}
	if events := breakGlassApprovalEvents(t, pool); !slices.Equal(events, []string{
		platformconfig.BreakGlassApprovalOffRequested, platformconfig.BreakGlassApprovalOnRestored,
	}) {
		t.Fatalf("events = %v", events)
	}
}

// A caller without access.breakglass.manage neither opens the values nor
// sees the section.
func TestBreakGlassSectionNeedsItsPermission(t *testing.T) {
	pool := newPATExpiryCorpusPool(t)
	router := breakGlassRouter(t, pool, grantingResolver(legacyAdministrator...))
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"values": map[string]any{platformconfig.KeyBreakGlassOnlineMaxMinutes: 60}}
		}
		if recorder := configDo(t, router, method, breakGlassValuesPath, body); recorder.Code != http.StatusForbidden {
			t.Errorf("%s without access.breakglass.manage: %d, want 403", method, recorder.Code)
		}
	}
	listed := func(router chi.Router) bool {
		recorder := configDo(t, router, http.MethodGet, "/admin/plugin_config_schemas/administration", nil)
		var schema struct {
			Sections []struct {
				ID string `json:"id"`
			} `json:"sections"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &schema); err != nil {
			t.Fatalf("decode the schema list: %v", err)
		}
		return slices.ContainsFunc(schema.Sections, func(s struct {
			ID string `json:"id"`
		}) bool {
			return s.ID == platformconfig.SectionBreakGlass
		})
	}
	if listed(router) {
		t.Error("the schema list shows break_glass to a caller without access.breakglass.manage")
	}
	if !listed(breakGlassRouter(t, pool, breakGlassResolver(nil, adminaccess.AccessBreakglassManage))) {
		t.Error("the schema list hides break_glass from a holder of access.breakglass.manage")
	}
}
