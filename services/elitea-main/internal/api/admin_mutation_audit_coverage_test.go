package api_test

// Every administration mutation writes an admin audit row (ADR-0059,
// Verification; story R1.7, issue 2511).
//
// The audit middleware decides by path prefix (middleware.AuditedSurfaces),
// so a route mounted outside those prefixes, or one whose administration
// check sits in a handler, can change administration data without a row, and
// nothing said so. This test walks every mutation of the administration
// surface instead of the prefix list:
//
//   - every write entry (IsWrite) of the surface found by driving the composed
//     router (internal/api/adminsurface), each variant once: the action
//     table's selectors, the Configuration sections, the central settings
//     variants;
//   - every write of the central override (adminaccess.CentralOverridePairs),
//     once per (route, guarded configuration type);
//   - every write block of the separation matrix of the write paths
//     (testdata/separation_matrix_writes.txt).
//
// It drives each through the composed router as a caller who holds every
// registered administration permission, so the success path is what is
// asserted, and reads the rows the router's AuditRecorder received for that
// request. A mutation with no row fails, unless the reviewed exemption file
// (testdata/admin_mutation_audit_exemptions.reviewed) names it with a reason.
// A row must carry acting_roles and a credential_kind (ADR-0059 decision 8):
// the resolver records the roles on the request's own context, so a handler
// that resolves with a detached context writes a row without them.

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const auditExemptionsFile = "testdata/admin_mutation_audit_exemptions.reviewed"

// auditMutation is one request the coverage test sends.
type auditMutation struct {
	route   adminsurface.Route
	variant adminsurface.Variant
	// source names where the mutation was enumerated from.
	source string
}

func (m auditMutation) String() string {
	if m.variant.Name == "" {
		return m.route.String()
	}
	return fmt.Sprintf("%s (variant %q)", m.route, m.variant.Name)
}

// readAuditExemptions reads the reviewed file: "METHOD pattern: reason" per
// line, # comments. Every entry needs a written reason.
func readAuditExemptions(text string) (map[string]string, []string) {
	exemptions := map[string]string{}
	var problems []string
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		route, reason, ok := strings.Cut(line, ": ")
		reason = strings.TrimSpace(reason)
		if !ok || reason == "" || len(strings.Fields(route)) != 2 {
			problems = append(problems, fmt.Sprintf("line %d: want \"METHOD pattern: reason\" with a written reason: %q", n+1, line))
			continue
		}
		if _, dup := exemptions[route]; dup {
			problems = append(problems, fmt.Sprintf("line %d: %s is listed twice", n+1, route))
		}
		exemptions[route] = reason
	}
	return exemptions, problems
}

// auditCoverageFailures sends each mutation once through handler, granting
// every registered administration name, and answers one failure per mutation
// that wrote no audit row (unless exempt), per row with no acting_roles or no
// credential_kind, and per exemption no mutation used.
func auditCoverageFailures(handler http.Handler, mutations []auditMutation, opts adminsurface.Options,
	exemptions map[string]string) []string {
	var failures []string
	used := map[string]bool{}
	for _, m := range mutations {
		answer := adminsurface.Send(handler, m.route, m.variant, opts, adminaccess.Names())
		key := m.route.Method + " " + adminaccess.RoutePattern(m.route.Pattern)
		if len(answer.Audited) == 0 {
			if exemptions[key] != "" {
				used[key] = true
				continue
			}
			failures = append(failures, fmt.Sprintf(
				"%s (from %s) answered %d and wrote no admin audit row: put its prefix in "+
					"middleware.auditedSurfaces, or list it in %s with a reason",
				m, m.source, answer.Status, auditExemptionsFile))
			continue
		}
		if exemptions[key] != "" {
			used[key] = true
			failures = append(failures, fmt.Sprintf(
				"%s writes an admin audit row but %s exempts it: remove the exemption", m, auditExemptionsFile))
		}
		row := answer.Audited[len(answer.Audited)-1]
		if len(row.ActingRoles) == 0 {
			failures = append(failures, fmt.Sprintf(
				"%s wrote an admin audit row without acting_roles: the administration check did not resolve "+
					"with the request's context", m))
		}
		if row.CredentialKind == "" {
			failures = append(failures, fmt.Sprintf("%s wrote an admin audit row without a credential_kind", m))
		}
	}
	for key := range exemptions {
		if !used[key] {
			failures = append(failures, fmt.Sprintf(
				"%s is exempt in %s, but no administration mutation of that route was driven: remove it",
				key, auditExemptionsFile))
		}
	}
	slices.Sort(failures)
	return slices.Compact(failures)
}

// variantNamed answers the variant of route the options declare by name; the
// default variant is the zero one.
func variantNamed(opts adminsurface.Options, route adminsurface.Route, name string) (adminsurface.Variant, bool) {
	if name == "" {
		return adminsurface.Variant{}, true
	}
	for _, v := range opts.Variants[route.String()] {
		if v.Name == name {
			return v, true
		}
	}
	return adminsurface.Variant{}, false
}

// composedMutations enumerates the mutations of the composed router.
func composedMutations(t *testing.T, driven drivenSurface) []auditMutation {
	t.Helper()
	var mutations []auditMutation
	seen := map[string]bool{}
	add := func(m auditMutation) {
		key := m.route.String() + "\x00" + m.variant.Name + "\x00" + m.variant.Body
		if !seen[key] {
			seen[key] = true
			mutations = append(mutations, m)
		}
	}
	mounted := func(method, pattern string) (adminsurface.Route, bool) {
		for _, r := range driven.composed.Routes {
			if r.Method == method && adminaccess.RoutePattern(r.Pattern) == adminaccess.RoutePattern(pattern) {
				return r, true
			}
		}
		return adminsurface.Route{}, false
	}

	// 1. The write entries of the administration surface, each variant.
	for _, entry := range driven.result.Surface {
		if !entry.IsWrite() {
			continue
		}
		variant, ok := variantNamed(driven.opts, entry.Route, entry.Variant)
		if !ok {
			t.Errorf("%s: the surface names variant %q, which the options do not declare", entry.Route, entry.Variant)
			continue
		}
		add(auditMutation{route: entry.Route, variant: variant, source: "the administration surface"})
	}
	// 2. The writes of the central override, per guarded configuration type.
	for _, pair := range adminaccess.CentralOverridePairs() {
		if pair.Method == http.MethodGet {
			continue
		}
		route, ok := mounted(pair.Method, pair.Pattern)
		if !ok {
			t.Errorf("central override pair %s: the route is not mounted", pair)
			continue
		}
		variant := adminsurface.Variant{Name: "type=" + pair.ConfigurationType}
		if pair.Method == http.MethodPost {
			variant.Body = `{"elitea_title":"setting","type":"` + pair.ConfigurationType + `","data":{}}`
		}
		add(auditMutation{route: route, variant: variant, source: "the central override pairs"})
	}
	// 3. The write blocks of the separation matrix of the write paths.
	raw, err := os.ReadFile(separationMatrixWritesFixture)
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, " ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("%s: want a method, a route and an action: %q", separationMatrixWritesFixture, line)
		}
		blocks++
		if fields[0] == http.MethodGet {
			continue
		}
		route, ok := mounted(fields[0], fields[1])
		if !ok {
			t.Errorf("separation matrix write %s %s: the route is not mounted", fields[0], fields[1])
			continue
		}
		// The surface drove the route already; a matrix write whose route
		// the surface does not list is driven with the default request.
		if len(driven.result.Find(route.Method, route.Pattern)) == 0 {
			add(auditMutation{route: route, source: "the separation matrix of the write paths"})
		}
	}
	if blocks < 50 {
		t.Fatalf("read %d write blocks from %s; the matrix has more", blocks, separationMatrixWritesFixture)
	}
	return mutations
}

// separationMatrixWritesFixture is the file of the write-path separation
// matrix (separation_matrix_writes_test.go, package api).
const separationMatrixWritesFixture = "testdata/separation_matrix_writes.txt"

var auditCoverageOnce = sync.OnceValues(func() ([]string, error) {
	raw, err := os.ReadFile(auditExemptionsFile)
	if err != nil {
		return nil, err
	}
	_, problems := readAuditExemptions(string(raw))
	return problems, nil
})

func TestTheAuditExemptionListIsReviewed(t *testing.T) {
	t.Parallel()
	problems, err := auditCoverageOnce()
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Errorf("%s: %s", auditExemptionsFile, problem)
	}
	// The format check itself: an entry without a reason is refused.
	if _, problems := readAuditExemptions("POST /api/v2/x:\nPUT /api/v2/y: a reason\nPUT /api/v2/y: again\n"); len(problems) != 2 {
		t.Errorf("an entry with no reason and a duplicate must be refused; got %q", problems)
	}
}

// auditFixtureRouter mounts the shapes the coverage test must judge, behind
// the production audit middleware and its prefix list.
func auditFixtureRouter(resolver *adminsurface.Resolver, recorder audit.Recorder) (http.Handler, []adminsurface.Route) {
	inHandler := func(detached bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user, _ := auth.UserFromContext(r.Context())
			ctx := r.Context()
			if detached {
				// A context that does not carry the request's audit slot.
				ctx = adminsurface.DetachedContext(ctx)
			}
			resolution, err := resolver.ResolvePermissions(ctx, user, auth.PermissionModeAdministration, "")
			if err != nil || !slices.Contains(resolution.Permissions, adminaccess.RuntimePlugins) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := adminsurface.Principal()
			user.CredentialKind = auth.CredentialKindToken
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	r.Use(apimw.Audit(recorder))
	// Outside /admin, with an in-handler check, under no audited prefix: the
	// shape a prefix-based test misses.
	r.Post("/api/v2/elitea_core/fixture_unaudited", inHandler(false))
	// The same check under an audited prefix.
	r.Post("/api/v2/admin/fixture_audited", inHandler(false))
	// Under an audited prefix, but the check resolves with a detached
	// context: a row with no acting_roles.
	r.Put("/api/v2/admin/fixture_detached", inHandler(true))
	var routes []adminsurface.Route
	_ = chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, adminsurface.Route{Method: method, Pattern: pattern})
		return nil
	})
	return r, routes
}

func TestEveryAdministrationMutationWritesAnAuditRow(t *testing.T) {
	t.Parallel()

	t.Run("fixtures", func(t *testing.T) {
		t.Parallel()
		resolver := &adminsurface.Resolver{}
		handler, routes := auditFixtureRouter(resolver, adminsurface.RequestAudit())
		opts := adminsurface.Options{Registry: adminaccess.Names()}
		result := adminsurface.Drive(handler, resolver, routes, opts)
		var mutations []auditMutation
		for _, entry := range result.Surface {
			if entry.IsWrite() {
				mutations = append(mutations, auditMutation{route: entry.Route, source: "the fixture surface"})
			}
		}
		if len(mutations) != 3 {
			t.Fatalf("the fixture surface has %d writes, want 3: %+v", len(mutations), result.Surface)
		}
		failures := auditCoverageFailures(handler, mutations, opts, map[string]string{
			"DELETE /api/v2/admin/fixture_gone": "a route that is not mounted",
		})
		for _, want := range []string{
			"POST /api/v2/elitea_core/fixture_unaudited (from the fixture surface) answered 204 and wrote no admin audit row",
			// No administration resolution reached the row: neither field.
			"PUT /api/v2/admin/fixture_detached wrote an admin audit row without acting_roles",
			"PUT /api/v2/admin/fixture_detached wrote an admin audit row without a credential_kind",
			"DELETE /api/v2/admin/fixture_gone is exempt",
		} {
			if !slices.ContainsFunc(failures, func(f string) bool { return strings.HasPrefix(f, want) }) {
				t.Errorf("no failure starts with %q; got %q", want, failures)
			}
		}
		if len(failures) != 4 {
			t.Errorf("got %d failures %q; want exactly the four above", len(failures), failures)
		}
		// A reviewed exemption silences the unaudited route, and only it.
		failures = auditCoverageFailures(handler, mutations, opts, map[string]string{
			"POST /api/v2/elitea_core/fixture_unaudited": "the fixture's reviewed reason",
		})
		if len(failures) != 2 || !slices.ContainsFunc(failures, func(f string) bool {
			return strings.HasPrefix(f, "PUT /api/v2/admin/fixture_detached")
		}) || slices.ContainsFunc(failures, func(f string) bool { return strings.Contains(f, "fixture_unaudited") }) {
			t.Errorf("with the exemption: got %q; want the detached route only", failures)
		}
	})

	t.Run("composed router", func(t *testing.T) {
		t.Parallel()
		driven := drivenComposedSurface(t)
		raw, err := os.ReadFile(auditExemptionsFile)
		if err != nil {
			t.Fatal(err)
		}
		exemptions, _ := readAuditExemptions(string(raw))
		mutations := composedMutations(t, driven)
		if len(mutations) < 150 {
			t.Fatalf("enumerated %d administration mutations; the surface has more", len(mutations))
		}
		// A standing browser session: the credential an administrator
		// writes with, which records credential_kind form.
		opts := driven.opts
		opts.Authenticate = adminsurface.AuthenticateFormSession
		t.Logf("driving %d administration mutations", len(mutations))
		for _, failure := range auditCoverageFailures(driven.composed.Handler, mutations, opts, exemptions) {
			t.Error(failure)
		}
	})
}
