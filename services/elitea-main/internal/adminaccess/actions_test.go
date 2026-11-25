package adminaccess_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
)

const goldenActions = "testdata/actions.golden"

func fixtureAction(method, pattern string) adminaccess.Action {
	return adminaccess.Action{Method: method, Pattern: pattern, Permission: adminaccess.RuntimePlugins}
}

func TestActionTableBuilds(t *testing.T) {
	t.Parallel()
	table := adminaccess.ActionTable()
	if len(table.Entries()) < 100 {
		t.Fatalf("the action table has %d entries; the administration surface has more than 100 write routes",
			len(table.Entries()))
	}
	if _, err := adminaccess.NewActions(table.Entries(), adminaccess.Registry()); err != nil {
		t.Fatalf("the action table does not pass NewActions: %v", err)
	}
	entry, ok := adminaccess.ActionFor(http.MethodPost, adminaccess.AuthUsersActionPattern, "set_admin_role")
	if !ok || entry.Danger != adminaccess.DangerCritical || entry.Token != adminaccess.TokenNever || !entry.Reason {
		t.Errorf("ActionFor(set_admin_role) = %+v, %v; want a critical entry with a reason and token: never", entry, ok)
	}
	if _, ok := adminaccess.ActionFor(http.MethodPost, adminaccess.AuthUsersActionPattern, ""); ok {
		t.Error("ActionFor answered an unnamed action on a route that multiplexes actions")
	}
}

// TestActionForTakesTheRequestPattern pins that the pattern chi.Walk lists and
// the one RoutePattern() answers during a request find the same entry: a
// sub-router mounted at "/" leaves "/*/" in the walked pattern, and a trailing
// slash is dropped at request time.
func TestActionForTakesTheRequestPattern(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ method, walked, request string }{
		{http.MethodPut, "/api/v2/admin/gateway/*/budget-alerts", "/api/v2/admin/gateway/budget-alerts"},
		{http.MethodPost, "/api/v2/admin/gateway/providers/", "/api/v2/admin/gateway/providers"},
	} {
		if got := adminaccess.RoutePattern(c.walked); got != c.request {
			t.Errorf("RoutePattern(%q) = %q, want %q", c.walked, got, c.request)
		}
		walked, ok1 := adminaccess.ActionFor(c.method, c.walked, "")
		request, ok2 := adminaccess.ActionFor(c.method, c.request, "")
		if !ok1 || !ok2 || walked.Key() != request.Key() {
			t.Errorf("%q and %q do not find the same entry: %v %v", c.walked, c.request, ok1, ok2)
		}
	}
}

// TestActionForRequestIsTheAdministrationModeOnly pins that the user-facing
// project delete, the same pattern in the default mode, has no entry.
func TestActionForRequestIsTheAdministrationModeOnly(t *testing.T) {
	t.Parallel()
	const pattern = "/api/v2/projects/project/{mode}/{projectID}"
	if entry, ok := adminaccess.ActionForRequest(true, http.MethodDelete, pattern, ""); !ok ||
		entry.Danger != adminaccess.DangerCritical {
		t.Errorf("the administration project delete: %+v, %v; want the critical entry", entry, ok)
	}
	if _, ok := adminaccess.ActionForRequest(false, http.MethodDelete, pattern, ""); ok {
		t.Error("the default-mode project delete has an entry; only the administration mode is on the surface")
	}
	if _, ok := adminaccess.ActionForRequest(false, http.MethodPut, "/api/v2/admin/branding/administration", ""); !ok {
		t.Error("a pattern without {mode} lost its entry in ActionForRequest")
	}
}

func TestActionTableRefusesACriticalActionWithoutAnElevation(t *testing.T) {
	t.Parallel()
	critical := fixtureAction(http.MethodPost, "/fixture/critical")
	critical.Danger = adminaccess.DangerCritical
	_, err := adminaccess.NewActions([]adminaccess.Action{critical}, adminaccess.Registry())
	if err == nil || !strings.Contains(err.Error(), "declares no elevation value") {
		t.Fatalf("NewActions(critical, no elevation) = %v; want the missing elevation named", err)
	}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Danger == adminaccess.DangerCritical && entry.Elevation == "" {
			t.Errorf("%s is critical and declares no elevation", entry.Key())
		}
	}
}

func TestActionTableRefusesACriticalTokenWithoutAReason(t *testing.T) {
	t.Parallel()
	critical := fixtureAction(http.MethodPost, "/fixture/critical")
	critical.Danger = adminaccess.DangerCritical
	critical.Elevation = adminaccess.ElevationDeny
	critical.Token = adminaccess.TokenAllowed
	_, err := adminaccess.NewActions([]adminaccess.Action{critical}, adminaccess.Registry())
	if err == nil || !strings.Contains(err.Error(), "token: allowed without a written reason") {
		t.Fatalf("NewActions(critical, token allowed, no reason) = %v; want the missing reason named", err)
	}

	critical.TokenReason = "the fixture's reason"
	if _, err := adminaccess.NewActions([]adminaccess.Action{critical}, adminaccess.Registry()); err != nil {
		t.Fatalf("NewActions(critical, token allowed, with a reason) = %v; want it accepted", err)
	}

	defaulted := critical
	defaulted.Token, defaulted.TokenReason = "", ""
	table, err := adminaccess.NewActions([]adminaccess.Action{defaulted}, adminaccess.Registry())
	if err != nil {
		t.Fatal(err)
	}
	if got := table.Entries()[0].Token; got != adminaccess.TokenNever {
		t.Errorf("a critical action with no token rule defaulted to %q; want never", got)
	}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Danger == adminaccess.DangerCritical && entry.Token == adminaccess.TokenAllowed &&
			strings.TrimSpace(entry.TokenReason) == "" {
			t.Errorf("%s is critical and says token: allowed without a reason", entry.Key())
		}
	}
}

func TestActionTableRefusesABadEntry(t *testing.T) {
	t.Parallel()
	get := fixtureAction(http.MethodGet, "/fixture/read")
	get.Danger = adminaccess.DangerDangerous
	selectedGet := fixtureAction(http.MethodGet, "/fixture/read")
	selectedGet.Name, selectedGet.Selector = "x", adminaccess.Selector{In: adminaccess.SelectPath, Field: "x", Value: "x"}
	unknown := fixtureAction(http.MethodPost, "/fixture/unknown")
	unknown.Permission = "fixture.not.registered"
	unnamed := fixtureAction(http.MethodPost, "/fixture/multi")
	named := fixtureAction(http.MethodPost, "/fixture/multi")
	named.Name, named.Selector = "x", adminaccess.Selector{In: adminaccess.SelectBody, Field: "action", Value: "x"}
	half := fixtureAction(http.MethodPut, "/fixture/retention")
	half.Classifier, half.Name = adminaccess.ClassifyRetention, adminaccess.OutcomeShorten
	half.Danger, half.Elevation = adminaccess.DangerCritical, adminaccess.ElevationDeny
	misnamed := fixtureAction(http.MethodPost, "/fixture/misnamed")
	misnamed.Selector = adminaccess.Selector{In: adminaccess.SelectBody, Field: "action", Value: "a"}
	misnamed.Name = "b"
	pathless := fixtureAction(http.MethodPut, "/fixture/section")
	pathless.Selector = adminaccess.Selector{In: adminaccess.SelectPath, Field: "plugin", Value: "a"}
	pathless.Name = "a"
	reasoned := fixtureAction(http.MethodPost, "/fixture/reasoned")
	reasoned.TokenReason = "only a critical action that says allowed carries one"

	for name, entries := range map[string][]adminaccess.Action{
		"a dangerous GET":                        {get},
		"a GET with a selector":                  {selectedGet},
		"an unregistered permission":             {unknown},
		"an entry that appears twice":            {fixtureAction(http.MethodPost, "/fixture/x"), fixtureAction(http.MethodPost, "/fixture/x")},
		"an unnamed action beside a named one":   {unnamed, named},
		"a classified action missing an outcome": {half},
		"a name that is not the selector value":  {misnamed},
		"a path selector the pattern lacks":      {pathless},
		"a token reason on a non-critical row":   {reasoned},
	} {
		if _, err := adminaccess.NewActions(entries, adminaccess.Registry()); err == nil {
			t.Errorf("NewActions accepted %s", name)
		}
	}
}

// A read has an entry only when the ADR restricts it beyond its gate, and
// then it takes the elevation and token rules like a write (R2.17).
func TestARestrictedReadHasAnEntry(t *testing.T) {
	t.Parallel()
	read := fixtureAction(http.MethodGet, "/fixture/read")
	read.Elevation, read.Token = adminaccess.ElevationDeny, adminaccess.TokenNever
	table, err := adminaccess.NewActions([]adminaccess.Action{read}, adminaccess.Registry())
	if err != nil {
		t.Fatalf("NewActions(restricted GET) = %v; want it accepted", err)
	}
	if got := table.Entries()[0]; got.Reason || got.Danger != adminaccess.DangerNone {
		t.Fatalf("restricted GET = %+v; want no danger and no reason", got)
	}
	entry, ok := adminaccess.ActionFor(http.MethodGet, adminaccess.SealedCredentialsPattern, "")
	if !ok || entry.Permission != adminaccess.AccessBreakglassManage ||
		entry.Elevation != adminaccess.ElevationDeny || entry.Token != adminaccess.TokenNever {
		t.Fatalf("the sealed credential list's entry = %+v, %v; want access.breakglass.manage, deny, never", entry, ok)
	}
}

func TestEveryActionPermissionIsRegistered(t *testing.T) {
	t.Parallel()
	for _, entry := range adminaccess.ActionTable().Entries() {
		if _, ok := adminaccess.Lookup(entry.Permission); !ok {
			t.Errorf("%s requires %q, which is not in the permission table", entry.Key(), entry.Permission)
			continue
		}
		if entry.Danger != adminaccess.DangerNone && !entry.Reason {
			t.Errorf("%s is %s and asks for no reason", entry.Key(), entry.Danger)
		}
	}
}

// multiplexed names, for each route that multiplexes actions on its body,
// the handler whose switch lists the actions it accepts.
var multiplexed = []struct {
	method, pattern string
	file, function  string
	field           string
}{
	{
		method: http.MethodPost, pattern: adminaccess.AuthUsersActionPattern,
		file: "../api/v2/admin/users.go", function: "AuthUsersAction", field: "Action",
	},
}

// TestMultiplexedRouteHasOneEntryPerHandlerAction reads the action names from
// the handler's switch, so an action added to the handler without an entry
// fails here.
func TestMultiplexedRouteHasOneEntryPerHandlerAction(t *testing.T) {
	t.Parallel()
	bodySelected := map[string]bool{}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Selector.In == adminaccess.SelectBody {
			bodySelected[entry.Method+" "+entry.Pattern] = true
		}
	}
	for _, m := range multiplexed {
		route := m.method + " " + m.pattern
		delete(bodySelected, route)
		cases, err := switchCases(m.file, m.function, m.field)
		if err != nil {
			t.Fatal(err)
		}
		if len(cases) == 0 {
			t.Fatalf("%s: found no case of the switch on .%s in %s", m.file, m.field, m.function)
		}
		var declared []string
		for _, entry := range adminaccess.ActionTable().Route(m.method, m.pattern) {
			if entry.Selector.In != adminaccess.SelectBody || entry.Selector.Field != "action" {
				t.Errorf("%s: %s is not selected by the body field `action`", route, entry.Key())
				continue
			}
			if !slices.Contains(declared, entry.Selector.Value) {
				declared = append(declared, entry.Selector.Value)
			}
		}
		slices.Sort(cases)
		slices.Sort(declared)
		if !slices.Equal(cases, declared) {
			t.Errorf("%s: the handler %s accepts the actions %v; the action table declares %v",
				route, m.function, cases, declared)
		}
	}
	for route := range bodySelected {
		t.Errorf("%s selects actions on its body but is not listed in multiplexed: name its handler", route)
	}
}

// switchCases answers the string cases of the first `switch x.<field>` in the
// function of file.
func switchCases(file, function, field string) ([]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		return nil, err
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function || fn.Body == nil {
			continue
		}
		var cases []string
		var found bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || found {
				return !found
			}
			sel, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field {
				return true
			}
			found = true
			for _, stmt := range sw.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err == nil {
						cases = append(cases, value)
					}
				}
			}
			return false
		})
		return cases, nil
	}
	return nil, fmt.Errorf("%s: no function %s", file, function)
}

func days(n int) *int { return &n }

func TestRetentionAction(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		old, new adminaccess.RetentionBounds
		want     string
	}{
		{"maximum lowered", adminaccess.RetentionBounds{MaxAgeDays: days(90)}, adminaccess.RetentionBounds{MaxAgeDays: days(30)}, adminaccess.OutcomeShorten},
		{"forever to a maximum", adminaccess.RetentionBounds{}, adminaccess.RetentionBounds{MaxAgeDays: days(365)}, adminaccess.OutcomeShorten},
		{"minimum lowered", adminaccess.RetentionBounds{MinAgeDays: 30, MaxAgeDays: days(90)}, adminaccess.RetentionBounds{MinAgeDays: 7, MaxAgeDays: days(90)}, adminaccess.OutcomeShorten},
		{"maximum raised", adminaccess.RetentionBounds{MaxAgeDays: days(30)}, adminaccess.RetentionBounds{MaxAgeDays: days(90)}, adminaccess.OutcomeLengthen},
		{"a maximum to forever", adminaccess.RetentionBounds{MaxAgeDays: days(30)}, adminaccess.RetentionBounds{}, adminaccess.OutcomeLengthen},
		{"minimum raised", adminaccess.RetentionBounds{MinAgeDays: 7}, adminaccess.RetentionBounds{MinAgeDays: 30}, adminaccess.OutcomeLengthen},
		{"unchanged", adminaccess.RetentionBounds{MinAgeDays: 7, MaxAgeDays: days(30)}, adminaccess.RetentionBounds{MinAgeDays: 7, MaxAgeDays: days(30)}, adminaccess.OutcomeLengthen},
		{"overrides start to apply", adminaccess.RetentionBounds{MaxAgeDays: days(365)}, adminaccess.RetentionBounds{MaxAgeDays: days(365), OverridesApply: true}, adminaccess.OutcomeShorten},
		{"overrides stop applying", adminaccess.RetentionBounds{MaxAgeDays: days(365), OverridesApply: true}, adminaccess.RetentionBounds{MaxAgeDays: days(365)}, adminaccess.OutcomeLengthen},
	} {
		if got := adminaccess.RetentionAction(c.old, c.new); got != c.want {
			t.Errorf("%s: RetentionAction = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGlobalProviderUpdateAction(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		changed []string
		want    string
	}{
		{[]string{"api_base"}, adminaccess.OutcomeRoute},
		{[]string{"base_url"}, adminaccess.OutcomeRoute},
		{[]string{"endpoint"}, adminaccess.OutcomeRoute},
		{[]string{"api_key"}, adminaccess.OutcomeRoute},
		{[]string{"aws_secret_access_key"}, adminaccess.OutcomeRoute},
		{[]string{"auth_mode"}, adminaccess.OutcomeRoute},
		{[]string{"workload_identity"}, adminaccess.OutcomeRoute},
		{[]string{"vertex_credentials"}, adminaccess.OutcomeRoute},
		{[]string{"label", "api_key"}, adminaccess.OutcomeRoute},
		{[]string{"a_field_nobody_classified"}, adminaccess.OutcomeRoute},
		{[]string{"models"}, adminaccess.OutcomeRoute},
		{[]string{"name"}, adminaccess.OutcomeRoute},
		{[]string{"status_ok"}, adminaccess.OutcomeRoute},
		{[]string{"label"}, adminaccess.OutcomeSettings},
		{[]string{"Display_Name", "description"}, adminaccess.OutcomeSettings},
		{nil, adminaccess.OutcomeSettings},
	} {
		if got := adminaccess.GlobalProviderUpdateAction(c.changed); got != c.want {
			t.Errorf("GlobalProviderUpdateAction(%v) = %q, want %q", c.changed, got, c.want)
		}
	}
}

// TestIdentityProviderSaveAction classifies through the real TrustChanges, so
// a change to its field names reaches this classifier.
func TestIdentityProviderSaveAction(t *testing.T) {
	t.Parallel()
	live := identityproviders.Provider{
		Key: "corp", Kind: identityproviders.KindOIDC, DisplayName: "Corp", Enabled: true,
		OIDC: &identityproviders.OIDCDocument{Issuer: "https://idp.example", ClientID: "elitea"},
	}
	with := func(edit func(*identityproviders.Provider)) identityproviders.Provider {
		p := live
		doc := *live.OIDC
		p.OIDC = &doc
		edit(&p)
		return p
	}
	for _, c := range []struct {
		name          string
		before, after identityproviders.Provider
		want          string
	}{
		{"created", identityproviders.Provider{}, live, adminaccess.OutcomeTrust},
		{"issuer changed", live, with(func(p *identityproviders.Provider) { p.OIDC.Issuer = "https://evil.example" }), adminaccess.OutcomeTrust},
		{"turned off", live, with(func(p *identityproviders.Provider) { p.Enabled = false }), adminaccess.OutcomeDisable},
		{"turned on", with(func(p *identityproviders.Provider) { p.Enabled = false }), live, adminaccess.OutcomeTrust},
		{"turned off and issuer changed", live, with(func(p *identityproviders.Provider) {
			p.Enabled = false
			p.OIDC.Issuer = "https://evil.example"
		}), adminaccess.OutcomeTrust},
		{"display name", live, with(func(p *identityproviders.Provider) { p.DisplayName = "Corporate" }), adminaccess.OutcomeSettings},
	} {
		got := adminaccess.IdentityProviderSaveAction(identityproviders.TrustChanges(c.before, c.after), c.after.Enabled)
		if got != c.want {
			t.Errorf("%s: IdentityProviderSaveAction = %q, want %q", c.name, got, c.want)
		}
	}
	if got := adminaccess.IdentityProviderSaveAction([]string{"secret"}, true); got != adminaccess.OutcomeTrust {
		t.Errorf("a replaced secret classified %q, want trust", got)
	}
}

func TestGovernanceRowAction(t *testing.T) {
	t.Parallel()
	if got := adminaccess.GovernanceRowAction("guardrail"); got != adminaccess.OutcomeGuardrail {
		t.Errorf("guardrail row: %q", got)
	}
	for _, rowType := range []string{"routing_rule", "budget_alert", "egress_allowlist", ""} {
		if got := adminaccess.GovernanceRowAction(rowType); got != adminaccess.OutcomeOther {
			t.Errorf("%q row: %q, want other", rowType, got)
		}
	}
}

func TestMaintenanceAction(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		before, after bool
		want          string
	}{
		{false, true, adminaccess.OutcomeOn},
		{true, true, adminaccess.OutcomeOther},
		{true, false, adminaccess.OutcomeOther},
		{false, false, adminaccess.OutcomeOther},
	} {
		if got := adminaccess.MaintenanceAction(c.before, c.after); got != c.want {
			t.Errorf("MaintenanceAction(%v, %v) = %q, want %q", c.before, c.after, got, c.want)
		}
	}
}

func TestSuspensionAction(t *testing.T) {
	t.Parallel()
	if got := adminaccess.SuspensionAction(string(safety.ScopePlatform)); got != adminaccess.OutcomePlatform {
		t.Errorf("the platform scope classified %q", got)
	}
	for _, scope := range []string{"project", "agent", "toolkit_type"} {
		if got := adminaccess.SuspensionAction(scope); got != adminaccess.OutcomeScoped {
			t.Errorf("the %q scope classified %q, want scoped", scope, got)
		}
	}
}

func TestUserSuspendAction(t *testing.T) {
	t.Parallel()
	if adminaccess.UserSuspendAction(true) != adminaccess.OutcomeSuspend ||
		adminaccess.UserSuspendAction(false) != adminaccess.OutcomeUnsuspend {
		t.Error("UserSuspendAction does not follow the suspended field")
	}
}

// TestActionTableMatchesGolden pins every entry, so that a change of danger,
// reason, elevation or token rule shows in review. Regenerate with
// UPDATE_GOLDEN=1 and review the diff.
func TestActionTableMatchesGolden(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("# The administration action table (ADR-0059 R0.1c). Generated by\n")
	b.WriteString("# TestActionTableMatchesGolden: UPDATE_GOLDEN=1 go test ./internal/adminaccess/\n")
	b.WriteString("# method\troute\taction\tdanger\treason\televation\ttoken\n")
	for _, entry := range adminaccess.ActionTable().Entries() {
		action, danger := entry.Name, string(entry.Danger)
		if action == "" {
			action = "-"
		}
		if danger == "" {
			danger = "none"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%t\t%s\t%s\n",
			entry.Method, entry.Pattern, action, danger, entry.Reason, entry.Elevation, entry.Token)
	}
	got := b.String()
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(goldenActions), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenActions, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenActions)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenActions, err)
	}
	if got != string(want) {
		t.Errorf("the action table drifted from %s; review the change and regenerate with UPDATE_GOLDEN=1.\n%s",
			goldenActions, lineDiff(string(want), got))
	}
}

func lineDiff(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	var out []string
	for _, line := range wantLines {
		if !slices.Contains(gotLines, line) {
			out = append(out, "- "+line)
		}
	}
	for _, line := range gotLines {
		if !slices.Contains(wantLines, line) {
			out = append(out, "+ "+line)
		}
	}
	return strings.Join(out, "\n")
}

// The break_glass section's save is critical, refused under an elevation and
// never callable with a token (ADR-0059 R2.2a): an elevation never changes
// the controls of break-glass itself.
func TestBreakGlassSectionActionIsCriticalDenyNever(t *testing.T) {
	t.Parallel()
	entry, ok := adminaccess.ActionFor(http.MethodPut, adminaccess.ConfigurationValuesPattern, "break_glass")
	if !ok {
		t.Fatal("no action entry for the break_glass section")
	}
	if entry.Danger != adminaccess.DangerCritical || entry.Elevation != adminaccess.ElevationDeny ||
		entry.Token != adminaccess.TokenNever || entry.Permission != adminaccess.AccessBreakglassManage {
		t.Fatalf("break_glass entry = %+v; want critical, elevation deny, token never, access.breakglass.manage", entry)
	}
}
