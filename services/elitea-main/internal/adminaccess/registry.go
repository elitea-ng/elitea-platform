// Package adminaccess is the code registry of the administration plane
// (ADR-0059 decision 1, story R0.1).
//
// # The permission table
//
// Every administration-mode permission that elitea-main enforces is a row of
// the permission table, and every gate names it through a constant of this
// package. A row says which console area the permission belongs to, its class
// (read, write or approve), the legacy permission it narrows, whether it is a
// legacy name, who may grant it, whether it can be a token scope, whether a
// native device session keeps it and whether it admits its holder during a
// maintenance window.
//
// Before this package the names were bare strings: twenty literals in the
// router, more in the sub-routers and handlers, and a hand-kept list for the
// roles catalogue. Nothing could say which names exist.
//
// # What a permission row never carries
//
// Danger, whether a reason is required, the elevation rule and the refusal of
// tokens are properties of an ACTION, not of a permission: one permission
// shortens and lengthens a retention policy, suspends a user and assigns a
// role. They belong to the action table (story R0.1c), keyed by route and
// action. Do not add such a field here.
//
// # Shape
//
// The table is an immutable value built once at package initialisation by
// NewTable, which a test can also call with rows of its own. The package
// imports nothing from internal/api: handlers import the registry, never the
// reverse. There is no database registry (ADR-0059, "Data model
// constraints"): auth_core__role_permission.permission stays a VARCHAR(64)
// with no foreign key, and MaxNameLength holds the table to that width.
//
// # Adding a permission
//
// Add a constant, add its row to rows(), grant it in a NEW shared migration
// (router_permission_grant_gate_test.go fails on a gated name no migration
// grants) and classify it in entitlements.Capabilities or
// entitlements.NotGovernanceRelevant. See the backend development guide,
// "Administration permissions and the registry".
package adminaccess

import (
	"fmt"
	"sort"
)

// MaxNameLength is the width of auth_core__role_permission.permission
// (VARCHAR(64), 001_initial.sql). A longer name cannot be stored as a grant.
const MaxNameLength = 64

// The sunset date constants of ADR-0059 decision 2. The legacy names
// `runtime.plugins` and `configuration.governance` end on a date, not at a
// version number.
//
//   - ReplacementShippedOn is the day the last replacement merged.
//   - SunsetAfter is the first day the removal may merge, at least 90 days
//     after ReplacementShippedOn.
//
// Both are ISO dates (YYYY-MM-DD) once set. They stay empty until story R3.15
// sets both in one commit; scripts/ci/check-sunset.mjs (story R0.14b) reads
// them.
const (
	ReplacementShippedOn = ""
	SunsetAfter          = ""
)

// InheritEndsOn is the date on which an `inherit` token ends (ADR-0059
// decision 2 and decision 7, "`inherit`"): SunsetAfter, "" while it is not
// set. The token responses and the central listing state it (R1.3a); the
// later of this date and 30 days after the owner's notice is R4.2's.
func InheritEndsOn() string { return SunsetAfter }

// Area is one of the six console areas of ADR-0059 decision 6.
type Area string

// The six console areas.
const (
	AreaPeopleAccess     Area = "people_access"
	AreaProjects         Area = "projects"
	AreaModelsSpend      Area = "models_spend"
	AreaAgentsGovernance Area = "agents_governance"
	AreaSecurity         Area = "security"
	AreaPlatform         Area = "platform"
)

// Areas returns the six areas in console order.
func Areas() []Area {
	return []Area{
		AreaPeopleAccess, AreaProjects, AreaModelsSpend,
		AreaAgentsGovernance, AreaSecurity, AreaPlatform,
	}
}

// Class says what a permission lets its holder do. A POST that changes nothing
// is still `read`; a route is classed by its effect, not its method.
type Class string

// The three classes.
const (
	ClassRead    Class = "read"
	ClassWrite   Class = "write"
	ClassApprove Class = "approve"
)

// Grantable says who may add a permission to a role.
type Grantable string

const (
	// GrantableAny may be granted from the Roles matrix, under the
	// no-escalation rules of internal/adminaccess.
	GrantableAny Grantable = "any"
	// GrantableSeededOnly is granted by a seeding migration and by nothing
	// else. The registry test fails when no migration grants such a name.
	GrantableSeededOnly Grantable = "seeded_only"
)

// Permission is one row of the permission table.
type Permission struct {
	// Name is the string stored in auth_core__role_permission.permission.
	Name string
	// Area is the console area the permission belongs to.
	Area Area
	// Class is read, write or approve.
	Class Class
	// Narrows lists the legacy permissions this one narrows, empty for a
	// new capability. A gate that accepts this permission keeps accepting
	// every name listed here (ADR-0059 decision 2).
	Narrows []string
	// Legacy marks a name that ends on the sunset date: runtime.plugins and
	// configuration.governance (story R0.14a). Until then a legacy name keeps
	// working where it is granted, but the Roles matrix adds it to no role
	// that does not hold it (CheckDeprecated), the roles catalogue marks it
	// deprecated, and the gates that name it may only shrink
	// (internal/api/legacy_gate_ratchet_test.go).
	Legacy bool
	// Grantable is any or seeded_only.
	Grantable Grantable
	// Token says whether the permission can be a token scope.
	Token bool
	// NativeClient says whether a native device session (desktop, mobile)
	// keeps the permission (ADR-0059 decision 7, "Native sessions").
	NativeClient bool
	// Maintenance says whether a holder is admitted to every route during a
	// maintenance window (AdmitsDuringMaintenance). Any other administration
	// permission admits its holder only to the routes an administration
	// gate decides, which then decides as it does outside a window.
	Maintenance bool
}

// Table is an immutable permission table. The zero value is an empty table.
type Table struct {
	rows   []Permission
	byName map[string]int
}

// NewTable validates rows and returns them as a table sorted by name. It
// refuses an empty or over-long name, a duplicate, a row with no valid area,
// class or grantable value, and a Narrows entry that names no row of the table
// or the row itself.
func NewTable(rows []Permission) (Table, error) {
	sorted := make([]Permission, len(rows))
	for i, row := range rows {
		sorted[i] = row.clone()
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	byName := make(map[string]int, len(sorted))
	for i, row := range sorted {
		if err := validateRow(row); err != nil {
			return Table{}, err
		}
		if _, duplicate := byName[row.Name]; duplicate {
			return Table{}, fmt.Errorf("adminaccess: permission %q appears twice", row.Name)
		}
		byName[row.Name] = i
	}
	for _, row := range sorted {
		for _, legacy := range row.Narrows {
			if legacy == row.Name {
				return Table{}, fmt.Errorf("adminaccess: permission %q narrows itself", row.Name)
			}
			if _, ok := byName[legacy]; !ok {
				return Table{}, fmt.Errorf("adminaccess: permission %q narrows %q, which is not in the table",
					row.Name, legacy)
			}
		}
	}
	return Table{rows: sorted, byName: byName}, nil
}

func validateRow(row Permission) error {
	switch {
	case row.Name == "":
		return fmt.Errorf("adminaccess: a permission row has no name")
	case len(row.Name) > MaxNameLength:
		return fmt.Errorf("adminaccess: permission %q is %d characters, longer than the %d of auth_core__role_permission.permission",
			row.Name, len(row.Name), MaxNameLength)
	}
	switch row.Area {
	case AreaPeopleAccess, AreaProjects, AreaModelsSpend, AreaAgentsGovernance, AreaSecurity, AreaPlatform:
	case "":
		return fmt.Errorf("adminaccess: permission %q has no area", row.Name)
	default:
		return fmt.Errorf("adminaccess: permission %q has the unknown area %q", row.Name, row.Area)
	}
	switch row.Class {
	case ClassRead, ClassWrite, ClassApprove:
	case "":
		return fmt.Errorf("adminaccess: permission %q has no class", row.Name)
	default:
		return fmt.Errorf("adminaccess: permission %q has the unknown class %q", row.Name, row.Class)
	}
	switch row.Grantable {
	case GrantableAny, GrantableSeededOnly:
	default:
		return fmt.Errorf("adminaccess: permission %q has the grantable value %q, want %q or %q",
			row.Name, row.Grantable, GrantableAny, GrantableSeededOnly)
	}
	return nil
}

// Rows returns a copy of the rows, sorted by name.
func (t Table) Rows() []Permission {
	rows := make([]Permission, len(t.rows))
	for i, row := range t.rows {
		rows[i] = row.clone()
	}
	return rows
}

// clone copies a row, including its Narrows slice, so a caller cannot write
// the table through it.
func (p Permission) clone() Permission {
	p.Narrows = append([]string(nil), p.Narrows...)
	return p
}

// Names returns the permission names, sorted.
func (t Table) Names() []string {
	names := make([]string, len(t.rows))
	for i, row := range t.rows {
		names[i] = row.Name
	}
	return names
}

// Lookup returns the row of name.
func (t Table) Lookup(name string) (Permission, bool) {
	i, ok := t.byName[name]
	if !ok {
		return Permission{}, false
	}
	return t.rows[i].clone(), true
}

// registry is the table of this service. It is built once and never written.
//
// A bad row panics at initialisation, in every binary and test binary that
// links this package. That is deliberate: a table that cannot be trusted
// must not serve a gate, and a lazily built table would move the same failure
// to the first request. The panic names the row and the file to fix.
var registry = mustTable(rows())

func mustTable(rows []Permission) Table {
	table, err := NewTable(rows)
	if err != nil {
		panic(fmt.Sprintf("invalid permission table: fix the row in "+
			"services/elitea-main/internal/adminaccess/permissions.go: %v", err))
	}
	return table
}

// Registry returns the permission table of this service.
func Registry() Table { return registry }

// Lookup returns the registry row of name.
func Lookup(name string) (Permission, bool) { return registry.Lookup(name) }

// Names returns every registered permission name, sorted.
func Names() []string { return registry.Names() }

// LegacyNames returns the names the registry marks Legacy, sorted.
func LegacyNames() []string {
	var names []string
	for _, row := range registry.rows {
		if row.Legacy {
			names = append(names, row.Name)
		}
	}
	return names
}

// IsLegacy reports whether the registry marks name Legacy.
func IsLegacy(name string) bool {
	row, ok := registry.Lookup(name)
	return ok && row.Legacy
}

// HoldsAdministrationPermission reports whether permissions hold at least one
// registered administration permission. A string the registry does not know
// is no administration permission, whatever role carries it.
func HoldsAdministrationPermission(permissions []string) bool {
	for _, name := range permissions {
		if _, ok := registry.Lookup(name); ok {
			return true
		}
	}
	return false
}

// AdmitsDuringMaintenance reports whether permissions hold a name the
// registry marks Maintenance: the in-application maintenance bypass, which
// opens every route during a window (ADR-0059 decision 2, R0.4b). The
// maintenance gate and the platform_settings flag read ask it, so the two
// cannot disagree.
func AdmitsDuringMaintenance(permissions []string) bool {
	for _, name := range permissions {
		if row, ok := registry.Lookup(name); ok && row.Maintenance {
			return true
		}
	}
	return false
}
