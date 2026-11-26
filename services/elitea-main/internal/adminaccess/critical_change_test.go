package adminaccess_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// criticalRefusedUntilAnnounced are the critical entries an elevation may do
// by their value whose writer does not run the announcement yet, so an
// elevation is refused them (issue filed with ADR-0059 R2.8b).
var criticalRefusedUntilAnnounced = []string{
	"POST /api/v2/admin/gateway/governance guardrail",
	"PUT /api/v2/admin/gateway/governance/{id} guardrail",
	"DELETE /api/v2/admin/gateway/governance/{id} guardrail",
	"DELETE /api/v2/projects/project/{mode}/{projectID}",
}

// Every critical entry that an elevation is not denied by its value is
// either announced in the transaction of its change or listed as refused;
// every announced key names such an entry.
func TestEveryCriticalElevationActionIsAnnouncedOrRefused(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Danger != adminaccess.DangerCritical || entry.Elevation == adminaccess.ElevationDeny {
			continue
		}
		key := entry.Key()
		seen[key] = true
		announced := adminaccess.CriticalChangeAnnounced[key]
		refused := slices.Contains(criticalRefusedUntilAnnounced, key)
		if announced == refused {
			t.Errorf("%s: announced %v, listed as refused %v; want exactly one", key, announced, refused)
		}
	}
	for key := range adminaccess.CriticalChangeAnnounced {
		if !seen[key] {
			t.Errorf("CriticalChangeAnnounced names %q, which is no critical entry an elevation may do", key)
		}
	}
	for _, key := range criticalRefusedUntilAnnounced {
		if !seen[key] {
			t.Errorf("the refused list names %q, which is no critical entry an elevation may do", key)
		}
	}
}

func criticalEntry(t *testing.T, key string) adminaccess.Action {
	t.Helper()
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Key() == key {
			return entry
		}
	}
	t.Fatalf("no entry %s", key)
	return adminaccess.Action{}
}

type nopAppender struct{}

func (nopAppender) Append(context.Context, pgx.Tx, securityaudit.SecurityEvent) error { return nil }

func TestArmCriticalChange(t *testing.T) {
	announcer := &adminaccess.CriticalAnnouncer{Appender: nopAppender{}, Now: time.Now}
	elevated := adminaccess.ElevationCaller{Elevated: true, UserID: 7, SessionRef: 3,
		ElevationIDs: []string{"0b7c2d4e-0000-4000-8000-0000000e1e70"}, Announcer: announcer}
	route := criticalEntry(t, "PUT /api/v2/admin/gateway/providers/{configID} route")
	ctx := securityaudit.ContextWithTransactionHooks(context.Background())

	if refusal := adminaccess.ArmCriticalChange(ctx, route, elevated); refusal != nil {
		t.Fatalf("an announced critical entry: %v", refusal)
	}
	if !securityaudit.TransactionHooksPending(ctx) {
		t.Fatal("the announcement was not armed")
	}
	// No hook set on the context: nowhere to put the write, refused.
	if refusal := adminaccess.ArmCriticalChange(context.Background(), route, elevated); refusal == nil ||
		refusal.Code != adminaccess.CodeElevationRestricted {
		t.Fatalf("no hook set: %v, want %s", refusal, adminaccess.CodeElevationRestricted)
	}
	// No announcer: refused.
	without := elevated
	without.Announcer = nil
	if refusal := adminaccess.ArmCriticalChange(ctx, route, without); refusal == nil {
		t.Fatal("no announcer: armed")
	}
	// An unannounced critical entry: refused.
	project := criticalEntry(t, "DELETE /api/v2/projects/project/{mode}/{projectID}")
	if refusal := adminaccess.ArmCriticalChange(ctx, project, elevated); refusal == nil {
		t.Fatal("a critical entry whose writer does not announce was admitted")
	}
	// A standing holder, an unelevated caller and a non-critical entry arm
	// nothing and are not refused.
	standing := elevated
	standing.Standing = []string{route.Permission}
	plain := adminaccess.ElevationCaller{}
	fresh := securityaudit.ContextWithTransactionHooks(context.Background())
	for name, caller := range map[string]adminaccess.ElevationCaller{"standing": standing, "unelevated": plain} {
		if refusal := adminaccess.ArmCriticalChange(fresh, route, caller); refusal != nil {
			t.Errorf("%s: %v", name, refusal)
		}
	}
	settings := criticalEntry(t, "PUT /api/v2/admin/gateway/providers/{configID} settings")
	if refusal := adminaccess.ArmCriticalChange(fresh, settings, elevated); refusal != nil {
		t.Errorf("a non-critical entry: %v", refusal)
	}
	if securityaudit.TransactionHooksPending(fresh) {
		t.Fatal("something was armed")
	}
}
