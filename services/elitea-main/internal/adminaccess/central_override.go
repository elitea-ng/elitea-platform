package adminaccess

// The central override (ADR-0059 R0.6): a central permission that reaches a
// project the caller is not a member of.
//
// Two powers use it. R0.6a: the administration member routes place other
// people into any project (projects.members.manage, or the legacy
// configuration.users.users.* names); the bulk invite keeps its own invite
// permissions and is marked the same way. R0.6b: the five guarded
// project settings of any project, through projects.settings.view and
// projects.settings.manage, on the reviewed allowlist below and nothing else.
//
// Each such write marks its audit record `central_override`
// (MarkCentralOverride); the audit middleware keeps the mark only on a
// response below 400, so a refused or failed request is not one. The admin audit row stores it as central_override (shared 0268, R1.4a).

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// MarkCentralOverride marks the audit record of the request as a central
// write into a project the caller is not a member of. It is a no-op on a
// request that is not audited.
func MarkCentralOverride(ctx context.Context) {
	audit.Annotate(ctx, audit.Annotation{CentralOverride: true})
}

// IsProjectMember reports whether the user holds any role in the project.
func IsProjectMember(ctx context.Context, q Querier, userID int64, projectID int64) (bool, error) {
	var member bool
	err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role
               WHERE project_id = $1 AND user_id = $2)`, projectID, userID).Scan(&member)
	if err != nil {
		return false, fmt.Errorf("read project membership: %w", err)
	}
	return member, nil
}

/* ── R0.6b: the guarded project settings ───────────────────────────────── */

// The five guarded configuration types: the project settings whose writes
// take models.project_settings.edit inside a project
// (internal/api/v2/configurations/project_settings_access.go). The values
// are the type names of internal/application/configurations; the
// configurations package tests that the two lists agree.
var guardedSettingTypes = []string{
	"ai_disclosure",
	"browser",
	"cloud_executor",
	"computer_use",
	"worker_egress",
}

// GuardedSettingTypes returns the five guarded configuration types, sorted.
func GuardedSettingTypes() []string { return slices.Clone(guardedSettingTypes) }

// IsGuardedSettingType reports whether a configuration type is one of the
// five guarded settings. The match is exact: a type the allowlist does not
// name, in any spelling, is not one.
func IsGuardedSettingType(name string) bool { return slices.Contains(guardedSettingTypes, name) }

// The project configurations routes the central override reaches, relative to
// /api/v2/configurations. Each is registered with and without the legacy
// {mode} segment.
const configurationsPrefix = "/api/v2/configurations"

// OverrideRoute is one project configurations route of the override.
type OverrideRoute struct {
	Method  string
	Pattern string
	// Permission is the central permission that admits the caller on it:
	// projects.settings.view for the reads, projects.settings.manage for the
	// writes. projects.settings.manage also reads (a write permission reads).
	Permission string
}

// overrideRoutes are the routes, in the order of the reviewed file. The
// configuration type of a request is read from the `type` query (the list),
// the stored row (the detail, the update and the delete) or the body (the
// create, and the type an update asks to change to).
func overrideRoutes() []OverrideRoute {
	routes := []OverrideRoute{}
	for _, path := range []struct {
		method, path, permission string
	}{
		{http.MethodGet, "/configurations/{projectID}", ProjectsSettingsView},
		{http.MethodGet, "/configuration/{projectID}/{configID}", ProjectsSettingsView},
		{http.MethodPost, "/configurations/{projectID}", ProjectsSettingsManage},
		{http.MethodPut, "/configuration/{projectID}/{configID}", ProjectsSettingsManage},
		{http.MethodDelete, "/configuration/{projectID}/{configID}", ProjectsSettingsManage},
	} {
		withMode := strings.Replace(path.path, "{projectID}", "{mode}/{projectID}", 1)
		routes = append(routes,
			OverrideRoute{Method: path.method, Pattern: configurationsPrefix + path.path, Permission: path.permission},
			OverrideRoute{Method: path.method, Pattern: configurationsPrefix + withMode, Permission: path.permission})
	}
	return routes
}

// OverridePair is one (route, configuration type) pair of the allowlist.
type OverridePair struct {
	OverrideRoute
	ConfigurationType string
}

// String is the pair's line in testdata/central_override.reviewed.
func (p OverridePair) String() string {
	return p.Method + " " + p.Pattern + " " + p.ConfigurationType + " " + p.Permission
}

// CentralOverridePairs is the allowlist: every override route with every
// guarded setting type. A credential, model or toolkit configuration type is
// on no pair, so the override never reaches one.
//
// Adding a pair is a security review: central_override_test.go fails until
// the same line is in testdata/central_override.reviewed.
func CentralOverridePairs() []OverridePair { return slices.Clone(centralOverridePairs) }

var centralOverridePairs = func() []OverridePair {
	pairs := []OverridePair{}
	for _, route := range overrideRoutes() {
		for _, typeName := range guardedSettingTypes {
			pairs = append(pairs, OverridePair{OverrideRoute: route, ConfigurationType: typeName})
		}
	}
	return pairs
}()

// OverrideRoutePermissions are the central permissions that admit a caller on
// (method, pattern) when the configuration types allow it (OverrideAllows),
// or nil when the route is not one of the override. The caller needs any one
// of them; projects.settings.manage also reads.
func OverrideRoutePermissions(method, pattern string) []string {
	for _, pair := range centralOverridePairs {
		if pair.Method != method || pair.Pattern != pattern {
			continue
		}
		if pair.Permission == ProjectsSettingsView {
			return []string{ProjectsSettingsView, ProjectsSettingsManage}
		}
		return []string{pair.Permission}
	}
	return nil
}

// OverrideAllows reports whether every one of typeNames is on a pair of the
// allowlist for (method, pattern). No type at all allows nothing: the decision
// is made on the type the request reaches, and a request that names none
// reaches nothing the allowlist covers. The match is exact.
func OverrideAllows(method, pattern string, typeNames ...string) bool {
	if len(typeNames) == 0 {
		return false
	}
	for _, typeName := range typeNames {
		if !slices.ContainsFunc(centralOverridePairs, func(pair OverridePair) bool {
			return pair.Method == method && pair.Pattern == pattern && pair.ConfigurationType == typeName
		}) {
			return false
		}
	}
	return true
}
