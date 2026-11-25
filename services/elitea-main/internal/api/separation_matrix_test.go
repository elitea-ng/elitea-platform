package api_test

// The separation matrix (ADR-0059 decision 7, story R0.9a): the answer of
// every route of the administration surface for every built-in role, pinned
// in testdata/separation_matrix.txt. The console hides what a role cannot
// use; this file is what shows that the server refuses it.
//
// Each role is driven as its standing permission set from the registry
// (adminaccess.BuiltInRole.Set), through the recording resolver of
// internal/api/adminsurface: no database, and no elevation (the elevation
// columns are R2.8's).

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"
)

var updateSeparationMatrix = flag.Bool("update", false,
	"rewrite testdata/separation_matrix.txt from the composed router (UPDATE_GOLDEN=1 does the same)")

const separationMatrixFile = "testdata/separation_matrix.txt"

// separationRoles are the built-in roles of ADR-0059 decision 3, in the
// order of the file's columns.
var separationRoles = []string{
	adminaccess.RoleSuperAdmin,
	adminaccess.RoleAdmin,
	adminaccess.RoleLLMManager,
	adminaccess.RoleProjectAdministrator,
	adminaccess.RoleSecurityAuditor,
	adminaccess.RoleEmergencyAccess,
}

// pendingIssues are the issues a `pending` row may name: the story that owns
// the row. R0.9b (#2587) owns the role write paths, the project plane, the
// central-override pairs and the per-action identity rows; it removes every
// marker and empties this list.
var pendingIssues = []int{2587}

// The answer classes of the file. flagged and restricted are the elevated
// columns' only.
const (
	classAllowed    = "allowed"
	classRefused    = "refused"
	classFlagged    = "flagged"
	classRestricted = "restricted"
	noAction        = "-"
)

// separationElevations are the elevated columns (ADR-0059 R2.8a): a Form
// session with no standing set under one approved elevation of the level,
// whose role set the elevation gives, in the order of the file's columns.
var separationElevations = []string{adminaccess.ElevationLevelAdmin, adminaccess.ElevationLevelSuperAdmin}

func validElevatedClass(class string) bool {
	switch class {
	case classAllowed, classRefused, classFlagged, classRestricted:
		return true
	}
	return false
}

// matrixRow is one line of the file.
type matrixRow struct {
	Method, Pattern string
	// Action is the variant that drove the route ("action delete",
	// "section email", "maintenance_window"), noAction for the default one.
	Action string
	// Class is read, write or approve (routeClass).
	Class string
	// Pending is the issue that owns the row, 0 when the answers are pinned.
	Pending int
	// Answers holds one class per separationRoles entry.
	Answers []string
	// Elevated holds one class per separationElevations entry.
	Elevated []string
}

func (r matrixRow) key() string { return r.Method + " " + r.Pattern + " " + r.Action }

// answer is the class of role in the row.
func (r matrixRow) answer(role string) string {
	if i := slices.Index(separationRoles, role); i >= 0 && i < len(r.Answers) {
		return r.Answers[i]
	}
	return ""
}

func (r matrixRow) format() string {
	var roles string
	if r.Pending != 0 {
		roles = fmt.Sprintf("pending #%d", r.Pending)
	} else {
		roles = fmt.Sprintf("%-47s %-10s %-10s", strings.Join(r.Answers, " "), r.elevated(0), r.elevated(1))
	}
	return fmt.Sprintf("%-69s %-7s %s %s %s", roles, r.Class, r.Method, r.Pattern, r.Action)
}

// elevated is the class of the i-th elevated column, "" when absent.
func (r matrixRow) elevated(i int) string {
	if i < len(r.Elevated) {
		return r.Elevated[i]
	}
	return ""
}

const separationMatrixHeader = `# The separation matrix of the administration surface (ADR-0059 R0.9a, #2498).
#
# Generated; do not edit by hand except for a pending marker. Regenerate with
#   go test ./internal/api/ -run TestSeparationMatrixMatchesTheRouter -update
# CI runs without the flag and fails on any difference, so review every changed
# line: it is a route whose answer for a role changed.
#
# One line per (method, route, action) of the administration surface, as
# internal/api/adminsurface drives the composed router. The action is the
# variant that drove the route: a body action ("action delete"), a
# Configuration section ("section email"), a check the default request does
# not reach ("maintenance_window", "native_session", "moderation"), or "-".
#
# Each role is its STANDING permission set from the registry
# (internal/adminaccess builtin_roles.go), resolved for every
# administration-mode check of the request, with no elevation.
#
# The two elevated columns (ADR-0059 R2.8a, #2522) are a Form browser session
# with NO standing set under one approved elevation at the admin, then the
# super_admin level: every permission the request resolves is the level's
# role set, given by the elevation. They show what the action table's
# elevation values do (internal/adminaccess action_rows.go): the test also
# checks each such row against the registry.
#
# The classes:
#   allowed     the administration checks admitted the request. The answer is
#               not 401, 403 or the maintenance window's 503, or it is what a
#               holder of every registered permission gets (a later project
#               gate, a check that refuses any administrator). The handler may
#               still answer 4xx or 5xx: the test router has no database.
#   refused     an authorization check refused it: 401, 403 or the window's 503
#               where a holder of every permission gets another answer.
#   flagged     (elevated columns) allowed, and the admin audit row is marked
#               elevation_flagged for the review.
#   restricted  (elevated columns) 403 elevation_restricted: an elevation
#               cannot do the action.
#   pending #N  the row belongs to issue N, which pins it against a database.
#               R0.9b (#2587) owns: role assignment (the set_admin_role
#               action, the mode role writes), the Roles matrix save and sync,
#               role create, rename and delete, the project member writes (both
#               routes), the bulk invite, the project matrix, the SCIM group
#               binding writes, the identity provider writes, and every route
#               of the central project settings (/configurations, reads too).
#
# The class column: read for GET and HEAD; for a write, the class of the
# permission its action table entry names (write or approve), write without one.
# A write whose entry names a read permission reads read: an effect-free check
# (validate-cel), and the entitlement_export section (#3026). The assertion
# tests hold every method but GET and HEAD to the write rules whatever its class.
#
# The columns: super_admin admin llm_manager project_administrator
# security_auditor emergency_access, elevated admin, elevated super_admin,
# then class, method, route and action.
`

// routeClass is the class of a surface entry: read for a GET or HEAD, and
// for a write the class of the permission its action entry names.
func routeClass(entry adminsurface.Entry) string {
	if !entry.IsWrite() {
		return string(adminaccess.ClassRead)
	}
	for _, action := range adminaccess.ActionTable().Route(entry.Method, entry.Pattern) {
		if action.Selector.Value != entry.SelectorValue() {
			continue
		}
		if row, ok := adminaccess.Lookup(action.Permission); ok {
			return string(row.Class)
		}
	}
	return string(adminaccess.ClassWrite)
}

func entryAction(entry adminsurface.Entry) string {
	if entry.Variant == "" {
		return noAction
	}
	return entry.Variant
}

// refusal reports whether an answer is an authorization refusal.
func refusal(a adminsurface.Answer) bool {
	return a.Status == http.StatusUnauthorized || a.Status == http.StatusForbidden || a.Maintenance()
}

// classify answers the class of a request sent with a role's set, given the
// answers with every registered permission (full) and with none (none). A
// refusal that a holder of everything gets too is not the administration
// checks' (a project gate after them); a check that refuses a holder of any
// administration permission (the native token mint) is read against none.
// It answers an error when no grant changes the answer: the matrix cannot
// read such a route.
func classify(role, full, none adminsurface.Answer) (string, error) {
	baseline := full
	if refusal(full) {
		if !refusal(none) {
			baseline = none
		} else if full.Status == none.Status && full.Body == none.Body {
			return "", fmt.Errorf("every grant answers %d: no role's answer can be read", full.Status)
		}
	}
	// A refusal is the baseline's only when it is the same answer: a 403 of
	// the administration check and a 403 of a later project gate share the
	// status, not the body.
	if refusal(role) && (role.Status != baseline.Status || role.Body != baseline.Body) {
		return classRefused, nil
	}
	return classAllowed, nil
}

// classifyElevated answers the class of an elevated request: restricted for
// the restriction's 403, the role class otherwise, flagged when allowed and
// marked for the review.
func classifyElevated(answer, full, none adminsurface.Answer) string {
	if answer.Status == http.StatusForbidden && strings.Contains(answer.Body, `"elevation_restricted"`) {
		return classRestricted
	}
	class, err := classify(answer, full, none)
	if err != nil {
		return classRefused
	}
	if class == classAllowed && answer.Flagged {
		return classFlagged
	}
	return class
}

// variantFor answers the variant that drove an entry.
func variantFor(opts adminsurface.Options, entry adminsurface.Entry) adminsurface.Variant {
	for _, v := range opts.Variants[entry.String()] {
		if v.Name == entry.Variant {
			v.Body = entry.Body
			return v
		}
	}
	return adminsurface.Variant{Name: entry.Variant, Body: entry.Body}
}

// separationRoleSets answers the standing set of each separationRoles role.
func separationRoleSets() ([][]string, error) {
	var sets [][]string
	for _, name := range separationRoles {
		role, ok := adminaccess.LookupBuiltInRole(name)
		if !ok || !role.DeclaresSet() {
			return nil, fmt.Errorf("%s is not a built-in role with a declared set", name)
		}
		sets = append(sets, role.Set())
	}
	return sets, nil
}

// computeMatrix sends every entry of the surface once per role, and once
// with every and with no permission, and answers one row per entry. An entry
// whose answer cannot be read is answered in unreadable.
func computeMatrix(handler http.Handler, opts adminsurface.Options, result adminsurface.Result) (matrix, error) {
	sets, err := separationRoleSets()
	if err != nil {
		return matrix{}, err
	}
	var rows []matrixRow
	unreadable := map[string]string{}
	for _, entry := range result.Surface {
		variant := variantFor(opts, entry)
		row := matrixRow{Method: entry.Method, Pattern: entry.Pattern, Action: entryAction(entry), Class: routeClass(entry)}
		full := adminsurface.Send(handler, entry.Route, variant, opts, opts.Registry)
		none := adminsurface.Send(handler, entry.Route, variant, opts, nil)
		for _, set := range sets {
			class, err := classify(adminsurface.Send(handler, entry.Route, variant, opts, set), full, none)
			if err != nil {
				unreadable[row.key()] = err.Error()
				class = classRefused
			}
			row.Answers = append(row.Answers, class)
		}
		for _, level := range separationElevations {
			role, _ := adminaccess.LookupBuiltInRole(level)
			elevated := variant
			elevated.Elevation = &adminsurface.Elevation{Level: level, Path: adminaccess.ElevationPathApproved}
			row.Elevated = append(row.Elevated,
				classifyElevated(adminsurface.Send(handler, entry.Route, elevated, opts, role.Set()), full, none))
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b matrixRow) int { return strings.Compare(a.sortKey(), b.sortKey()) })
	return matrix{rows: rows, unreadable: unreadable}, nil
}

// matrix is a computed separation matrix: its rows, and the rows whose answer
// no grant changes, with the reason.
type matrix struct {
	rows       []matrixRow
	unreadable map[string]string
}

func (r matrixRow) sortKey() string { return r.Pattern + "\x00" + r.Method + "\x00" + r.Action }

// parseMatrix reads the file's rows.
func parseMatrix(text string) ([]matrixRow, error) {
	var rows []matrixRow
	for n, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		var row matrixRow
		switch {
		case len(fields) >= 6 && fields[0] == "pending":
			issue, err := strconv.Atoi(strings.TrimPrefix(fields[1], "#"))
			if err != nil || !strings.HasPrefix(fields[1], "#") {
				return nil, fmt.Errorf("line %d: a pending marker names an issue as #N: %q", n+1, line)
			}
			row.Pending, fields = issue, fields[2:]
		case len(fields) >= len(separationRoles)+4:
			row.Answers, fields = fields[:len(separationRoles)], fields[len(separationRoles):]
			for _, answer := range row.Answers {
				if answer != classAllowed && answer != classRefused {
					return nil, fmt.Errorf("line %d: %q is not a class", n+1, answer)
				}
			}
			// The elevated classes are never a route class (read, write,
			// approve), so a row written without them still parses, and
			// answerFailures names each missing one.
			for len(row.Elevated) < len(separationElevations) && len(fields) > 4 && validElevatedClass(fields[0]) {
				row.Elevated, fields = append(row.Elevated, fields[0]), fields[1:]
			}
		default:
			return nil, fmt.Errorf("line %d: want %d classes (and %d elevated ones) or a pending marker, a class, "+
				"a method, a route and an action: %q", n+1, len(separationRoles), len(separationElevations), line)
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("line %d: no class, method, route and action: %q", n+1, line)
		}
		row.Class, row.Method, row.Pattern, row.Action = fields[0], fields[1], fields[2], strings.Join(fields[3:], " ")
		rows = append(rows, row)
	}
	return rows, nil
}

func formatMatrix(rows []matrixRow) string {
	var b strings.Builder
	b.WriteString(separationMatrixHeader)
	for _, row := range rows {
		b.WriteString(row.format())
		b.WriteByte('\n')
	}
	return b.String()
}

// coverageFailures answers one message per surface row the file has no row
// for, and per file row the surface does not have.
func coverageFailures(surface, file []matrixRow) []string {
	inFile := map[string]bool{}
	for _, row := range file {
		inFile[row.key()] = true
	}
	inSurface := map[string]bool{}
	var failures []string
	for _, row := range surface {
		inSurface[row.key()] = true
		if !inFile[row.key()] {
			failures = append(failures, fmt.Sprintf(
				"%s is on the administration surface and has no row in %s: regenerate it with -update and "+
					"review the new row", row.key(), separationMatrixFile))
		}
	}
	for _, row := range file {
		if !inSurface[row.key()] {
			failures = append(failures, fmt.Sprintf(
				"%s has a row in %s but is not on the administration surface: regenerate the file", row.key(), separationMatrixFile))
		}
	}
	return failures
}

// pendingFailures answers one message per pending row whose issue is not in
// allowed.
func pendingFailures(file []matrixRow, allowed []int) []string {
	var failures []string
	for _, row := range file {
		if row.Pending != 0 && !slices.Contains(allowed, row.Pending) {
			failures = append(failures, fmt.Sprintf(
				"%s is pending on #%d, which is not an issue that owns separation matrix rows (%v); "+
					"pin its answers instead", row.key(), row.Pending, allowed))
		}
	}
	return failures
}

// answerFailures answers one message per (row, role) whose class in the file
// differs from the computed one. Pending rows are skipped.
func answerFailures(computed, file []matrixRow) []string {
	want := map[string]matrixRow{}
	for _, row := range file {
		want[row.key()] = row
	}
	var failures []string
	for _, got := range computed {
		row, ok := want[got.key()]
		if !ok || row.Pending != 0 {
			continue
		}
		if row.Class != got.Class {
			failures = append(failures, fmt.Sprintf("%s: the file says class %s, the router %s", got.key(), row.Class, got.Class))
		}
		for _, role := range separationRoles {
			if row.answer(role) != got.answer(role) {
				failures = append(failures, fmt.Sprintf("%s: %s is %s in the file and %s by the router",
					got.key(), role, row.answer(role), got.answer(role)))
			}
		}
		for i, level := range separationElevations {
			if row.elevated(i) != got.elevated(i) {
				failures = append(failures, fmt.Sprintf("%s: elevated %s is %s in the file and %s by the router",
					got.key(), level, row.elevated(i), got.elevated(i)))
			}
		}
	}
	return failures
}

// withPending carries the pending markers of file over to computed.
func withPending(computed, file []matrixRow) []matrixRow {
	pending := map[string]int{}
	for _, row := range file {
		if row.Pending != 0 {
			pending[row.key()] = row.Pending
		}
	}
	out := slices.Clone(computed)
	for i := range out {
		if issue := pending[out[i].key()]; issue != 0 {
			out[i].Pending, out[i].Answers, out[i].Elevated = issue, nil, nil
		}
	}
	return out
}

func readSeparationMatrix(t *testing.T) []matrixRow {
	t.Helper()
	data, err := os.ReadFile(separationMatrixFile)
	if err != nil {
		t.Fatalf("read %s: %v (generate it with -update)", separationMatrixFile, err)
	}
	rows, err := parseMatrix(string(data))
	if err != nil {
		t.Fatalf("%s: %v", separationMatrixFile, err)
	}
	if len(rows) < 200 {
		t.Fatalf("%s has %d rows; the administration surface has more than 200", separationMatrixFile, len(rows))
	}
	return rows
}

var composedMatrixOnce = sync.OnceValues(func() (matrix, error) {
	driven, err := composedSurface()
	if err != nil {
		return matrix{}, err
	}
	return computeMatrix(driven.composed.Handler, driven.opts, driven.result)
})

// composedMatrix is the matrix of the composed router, computed once.
func composedMatrix(t *testing.T) matrix {
	t.Helper()
	drivenComposedSurface(t) // its floor on the number of routes driven
	m, err := composedMatrixOnce()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSeparationMatrixCoversTheAdministrationSurface fails when a route of
// the administration surface has no row in the file, and on a row whose route
// left the surface. The fixture is a route outside /admin whose only
// administration check is in its handler.
func TestSeparationMatrixCoversTheAdministrationSurface(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) {
		resolver := &adminsurface.Resolver{}
		handler, routes := fixtureRouter(resolver)
		opts := adminsurface.Options{Registry: adminaccess.Names(), OtherModes: []string{"models.fixture.view"}}
		result := adminsurface.Drive(handler, resolver, routes, opts)
		fixture, err := computeMatrix(handler, opts, result)
		if err != nil {
			t.Fatal(err)
		}
		computed := fixture.rows
		const budget = "PUT /api/v2/elitea_core/budget/{projectID} -"
		idx := slices.IndexFunc(computed, func(r matrixRow) bool { return r.key() == budget })
		if idx < 0 {
			t.Fatalf("the fixture surface has no row %q; got %d rows", budget, len(computed))
		}
		// The in-handler check accepts configuration.governance alone, which
		// both administrator roles hold and no narrow role does.
		if got := computed[idx].Answers; !slices.Equal(got, []string{
			classAllowed, classAllowed, classRefused, classRefused, classRefused, classRefused}) {
			t.Errorf("%s: got %v, want the two administrator roles allowed and the rest refused", budget, got)
		}
		without := slices.Delete(slices.Clone(computed), idx, idx+1)
		failures := coverageFailures(computed, without)
		if len(failures) != 1 || !strings.HasPrefix(failures[0], budget+" is on the administration surface") {
			t.Errorf("a file without the in-handler route: got %q, want one failure naming %s", failures, budget)
		}
		stale := append(slices.Clone(computed), matrixRow{Method: http.MethodGet, Pattern: "/api/v2/admin/gone", Action: noAction})
		if failures := coverageFailures(computed, stale); len(failures) != 1 ||
			!strings.Contains(failures[0], "is not on the administration surface") {
			t.Errorf("a file with a row the surface lacks: got %q, want one failure", failures)
		}
		// The formatted file reads back as itself.
		parsed, err := parseMatrix(formatMatrix(computed))
		if err != nil || len(coverageFailures(computed, parsed)) != 0 || len(answerFailures(computed, parsed)) != 0 {
			t.Errorf("the formatted fixture matrix does not read back: %v", err)
		}
	})

	t.Run("composed router", func(t *testing.T) {
		for _, failure := range coverageFailures(composedMatrix(t).rows, readSeparationMatrix(t)) {
			t.Error(failure)
		}
	})
}

// TestSeparationMatrixMatchesTheRouter: the class of every (route, role)
// equals the file. With -update (or UPDATE_GOLDEN=1) it rewrites the file
// instead, keeping its pending markers.
func TestSeparationMatrixMatchesTheRouter(t *testing.T) {
	m := composedMatrix(t)
	computed, unreadable := m.rows, m.unreadable
	if *updateSeparationMatrix || os.Getenv("UPDATE_GOLDEN") == "1" {
		var existing []matrixRow
		if data, err := os.ReadFile(separationMatrixFile); err == nil {
			if existing, err = parseMatrix(string(data)); err != nil {
				t.Fatalf("%s: %v", separationMatrixFile, err)
			}
		}
		if err := os.WriteFile(separationMatrixFile, []byte(formatMatrix(withPending(computed, existing))), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", separationMatrixFile)
		return
	}
	file := readSeparationMatrix(t)
	for _, failure := range answerFailures(computed, file) {
		t.Error(failure)
	}
	// A route no grant changes cannot be pinned; only a row its owning
	// story pins may be one.
	pending := map[string]bool{}
	for _, row := range file {
		pending[row.key()] = row.Pending != 0
	}
	for key, reason := range unreadable {
		if !pending[key] {
			t.Errorf("%s: %s; reach the decision (a variant), or mark the row pending on the story that pins it", key, reason)
		}
	}
	data, err := os.ReadFile(separationMatrixFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := formatMatrix(withPending(computed, file)); string(data) != want && !t.Failed() {
		t.Errorf("%s is not in the generated form (order, spacing or header); regenerate it with -update", separationMatrixFile)
	}
}

// TestSeparationMatrixPendingRowsNameAnOwningIssue: a pending row names an
// issue of pendingIssues, and that list holds R0.9b (#2587) only.
func TestSeparationMatrixPendingRowsNameAnOwningIssue(t *testing.T) {
	if !slices.Equal(pendingIssues, []int{2587}) {
		t.Errorf("pendingIssues is %v; only R0.9b (#2587) owns pending rows", pendingIssues)
	}
	fixture, err := parseMatrix(separationMatrixHeader +
		"pending #2587 write POST /api/v2/admin/roles/{scope}/{mode} -\n" +
		"pending #9999 write POST /api/v2/admin/fixture -\n")
	if err != nil {
		t.Fatal(err)
	}
	if failures := pendingFailures(fixture, pendingIssues); len(failures) != 1 ||
		!strings.HasPrefix(failures[0], "POST /api/v2/admin/fixture - is pending on #9999") {
		t.Errorf("got %q, want one failure naming the row pending on #9999", failures)
	}
	for _, failure := range pendingFailures(readSeparationMatrix(t), pendingIssues) {
		t.Error(failure)
	}
}

// isWrite reports whether a row can change something: a method other than
// GET and HEAD, or a write or approve class. A row is held to the write
// assertions by its effect as well as its class, so a PATCH behind a read
// permission (the Marketplace moderation) is a write here.
func (r matrixRow) isWrite() bool {
	return (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		r.Class == string(adminaccess.ClassWrite) || r.Class == string(adminaccess.ClassApprove)
}

// rowsMatching answers the rows of file whose "method route action"
// contains any of markers, the writes only when writes is set. Pending rows
// are kept: roleAssertion.refused reads them from the router.
func rowsMatching(file []matrixRow, writes bool, markers ...string) []matrixRow {
	var out []matrixRow
	for _, row := range file {
		if writes && !row.isWrite() {
			continue
		}
		if slices.ContainsFunc(markers, func(m string) bool { return strings.Contains(row.key(), m) }) {
			out = append(out, row)
		}
	}
	return out
}

// actionArea answers the console area of the permission the action table
// names for a write row, "" for a read or a write without an entry.
func actionArea(row matrixRow) adminaccess.Area {
	selector := ""
	if row.Action != noAction {
		_, selector, _ = strings.Cut(row.Action, " ")
	}
	for _, action := range adminaccess.ActionTable().Route(row.Method, row.Pattern) {
		if action.Selector.Value != selector {
			continue
		}
		if permission, ok := adminaccess.Lookup(action.Permission); ok {
			return permission.Area
		}
	}
	return ""
}

// roleAssertion reads the file and, for a pending row, the router: a pending
// row has no answers in the file, but its gate is still the role's. A row
// whose answer the router cannot read either (the central-override pairs) is
// R0.9b's alone.
type roleAssertion struct {
	t      *testing.T
	file   []matrixRow
	router map[string]matrixRow
}

func newRoleAssertion(t *testing.T) roleAssertion {
	t.Helper()
	m := composedMatrix(t)
	router := map[string]matrixRow{}
	for _, row := range m.rows {
		if _, unreadable := m.unreadable[row.key()]; !unreadable {
			router[row.key()] = row
		}
	}
	return roleAssertion{t: t, file: readSeparationMatrix(t), router: router}
}

// refused fails on a row of rows where role is not refused, and when no row
// of rows could be read: an assertion over no row proves nothing.
func (a roleAssertion) refused(role, category string, rows []matrixRow) {
	a.t.Helper()
	checked := 0
	for _, row := range rows {
		source := "the file"
		if row.Pending != 0 {
			pinned, ok := a.router[row.key()]
			if !ok {
				continue
			}
			row, source = pinned, "the router (the row is pending on #"+strconv.Itoa(row.Pending)+")"
		}
		checked++
		if row.answer(role) != classRefused {
			a.t.Errorf("%s: %s is %s on %s in %s", category, role, row.answer(role), row.key(), source)
		}
	}
	if checked == 0 {
		a.t.Errorf("%s: no row of %s could be read; the assertion would prove nothing", category, separationMatrixFile)
	}
}

// TestSeparationMatrixLLMManagerIsRefusedOutsideModelsAndSpend: llm_manager is
// refused on every identity, role, user, agent registry, delegation review,
// worker egress, kill switch, retention and guardrail write route. A row of
// these that is pending on R0.9b is read from the router: llm_manager holds no
// permission their gates accept, so the gate answer is the whole answer.
func TestSeparationMatrixLLMManagerIsRefusedOutsideModelsAndSpend(t *testing.T) {
	a := newRoleAssertion(t)
	for category, markers := range map[string][]string{
		"identity": {"/identity_providers/", "/scim_clients/", "/native_clients/", "/native_devices/",
			"/microsoft_oauth_apps", "section auth"},
		"role":              {"/admin/roles/", "/admin/permissions/", "/admin/modes/", "/user_project_permissions/"},
		"user":              {"/auth_users/", "/user_suspend/", "/user_invite/", "/invites_bulk/", "/admin/users/"},
		"agent registry":    {"/agent_identities", "/external_agent_identities", "/admin/mandates", "/mandate_attachments/"},
		"delegation review": {"/delegation_review", "section delegation"},
		"worker egress":     {"/worker_egress", "section worker_egress"},
		"kill switch":       {"/execution_suspensions"},
		"retention":         {"/retention/", "/legal_holds"},
		"guardrail":         {"section guardrails"},
	} {
		a.refused(adminaccess.RoleLLMManager, category, rowsMatching(a.file, true, markers...))
	}
}

// TestSeparationMatrixProjectAdministratorIsRefusedOnPlatformAndLLMWrites:
// project_administrator is refused on every platform configuration route and
// every LLM write route: the writes whose action names a models and spend
// permission, wherever they are mounted. The project-role matrix edit and the
// SCIM group bindings are asserted in R0.9b.
func TestSeparationMatrixProjectAdministratorIsRefusedOnPlatformAndLLMWrites(t *testing.T) {
	a := newRoleAssertion(t)
	platform := rowsMatching(a.file, false, "/plugin_config_values/", "/plugin_config_schemas/",
		"/plugin_config_suggestions/", "/admin/branding/", "/admin/email/", "/admin/maintenance/",
		"/admin/platform_routines/", "/admin/native_clients/", "/admin/toolkit_types/", "/admin/mcp_prebuilt_servers/")
	llm := rowsMatching(a.file, true, "/admin/gateway/", "/project_budget/", "/user_budget/")
	for _, row := range a.file {
		switch actionArea(row) {
		case adminaccess.AreaPlatform:
			platform = append(platform, row)
		case adminaccess.AreaModelsSpend:
			llm = append(llm, row)
		}
	}
	a.refused(adminaccess.RoleProjectAdministrator, "platform configuration", platform)
	a.refused(adminaccess.RoleProjectAdministrator, "LLM write", llm)
}

// TestSeparationMatrixSecurityAuditorIsRefusedOnEveryWrite: security_auditor
// is refused on every route that writes or approves (the break-glass review
// attestation is the exception R2.9 adds with its route) and on the run
// evidence routes until #2429 adds the central read.
func TestSeparationMatrixSecurityAuditorIsRefusedOnEveryWrite(t *testing.T) {
	a := newRoleAssertion(t)
	var writes []matrixRow
	for _, row := range a.file {
		if row.isWrite() {
			writes = append(writes, row)
		}
	}
	a.refused(adminaccess.RoleSecurityAuditor, "write or approve", writes)
	// No route of the surface reads run evidence yet (#2429); a row that does
	// must refuse the auditor until that story pins it.
	for _, row := range rowsMatching(a.file, false, "evidence") {
		if row.Pending == 0 && row.answer(adminaccess.RoleSecurityAuditor) != classRefused {
			t.Errorf("run evidence: security_auditor is %s on %s before #2429",
				row.answer(adminaccess.RoleSecurityAuditor), row.key())
		}
	}
}
