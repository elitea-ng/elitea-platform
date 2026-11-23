package adminsurface

import (
	"fmt"
	"slices"
	"strings"
)

// IsAdministrationRoute reports whether a route sits under a known
// administration prefix: it is /api/v2/admin or below it, one of its literal
// segments is `administration`, or it has a `{mode}` segment, with or without
// a pattern (Drive sends `administration` there). A route of this shape that
// Drive could not place in the surface must not read as a route outside it.
//
// A mode carried in the query or the body is not a path shape and is not
// recognised; such a route is driven with a Variant.
func IsAdministrationRoute(r Route) bool {
	if r.Pattern == "/api/v2/admin" || strings.HasPrefix(r.Pattern, "/api/v2/admin/") {
		return true
	}
	for _, segment := range strings.Split(r.Pattern, "/") {
		if segment == "administration" || placeholderName(segment) == "mode" {
			return true
		}
	}
	return false
}

// claimsAdministration reports whether a route's literal path names the
// administration plane: /api/v2/admin or below it, or a literal
// `administration` segment. Such a route that decides in another mode only is
// reported. A `{mode}` route is not: its twins in the other modes decide in
// the default mode by design (the Configuration and skill twins).
func claimsAdministration(r Route) bool {
	if r.Pattern == "/api/v2/admin" || strings.HasPrefix(r.Pattern, "/api/v2/admin/") {
		return true
	}
	return slices.Contains(strings.Split(r.Pattern, "/"), "administration")
}

// placeholderName answers the name of a `{name}` or `{name:pattern}` segment,
// or "" for a literal one.
func placeholderName(segment string) string {
	if m := placeholder.FindStringSubmatch(segment); m != nil && m[0] == segment {
		return m[1]
	}
	return ""
}

// UnreachedFailures answers one message per route under a known
// administration prefix that Drive could not place in the surface (no
// decision at all, or decisions in other modes only), per route that timed out
// or answered unstably wherever it is mounted, and per entry of declared that
// is stale. declared maps Route.String() to the reason a route of that shape
// makes no administration decision on purpose (a deliberately open route, a
// project-scoped gate). An entry is stale when its route was placed in the
// surface, or is not mounted at all.
func (res Result) UnreachedFailures(declared map[string]string) []string {
	var failures []string
	unreached := map[string]bool{}
	for _, u := range res.Unreached {
		unreached[u.String()] = true
		switch {
		case u.TimedOut:
			failures = append(failures, fmt.Sprintf(
				"%s (variant %q) did not answer within the timeout; a timed-out answer is no evidence", u, u.Variant))
			continue
		case u.Unstable:
			failures = append(failures, fmt.Sprintf(
				"%s (variant %q) answers the same request differently; the probe cannot read it", u, u.Variant))
			continue
		}
		if !IsAdministrationRoute(u.Route) {
			continue
		}
		if _, ok := declared[u.String()]; ok {
			continue
		}
		decided := "made no permission decision"
		if len(u.Modes) > 0 {
			decided = fmt.Sprintf("decided only in the %s mode(s), never in administration", strings.Join(u.Modes, ", "))
		}
		failures = append(failures, fmt.Sprintf(
			"%s (variant %q) is under an administration prefix but %s: it answered %d %s. "+
				"Give the route what it needs to reach its decision (a variant, a nil-safe store), "+
				"or declare why it makes none",
			u, u.Variant, decided, u.Status, u.Answer))
	}
	for route := range declared {
		if !unreached[route] {
			failures = append(failures, fmt.Sprintf(
				"%s is declared as making no administration decision, but it made one or is not mounted; remove the entry", route))
		}
	}
	slices.Sort(failures)
	return failures
}

// InvisibleKey is the key of an invisible check in RegistryFailures's
// allowance: the route and the function that resolved.
func InvisibleKey(route Route, caller string) string { return route.String() + " via " + caller }

// RegistryFailures answers one message per administration-mode check that
// names a permission outside registry, per check whose outcome no registered
// name changes (KindInvisible) unless invisible holds its InvisibleKey with a
// reason, per stale entry of invisible, and per orphan resolution.
//
// A check that accepts a registered name AND an unregistered one is
// indistinguishable here from one that accepts only the registered name;
// TestNoAdministrationPermissionLiteralOutsideTheRegistry reads the source
// for that case.
func (res Result) RegistryFailures(registry []string, invisible map[string]string) []string {
	known := map[string]bool{}
	for _, name := range registry {
		known[name] = true
	}
	var failures []string
	allowed := map[string]bool{}
	for _, entry := range res.Surface {
		for _, check := range entry.Checks {
			for _, name := range check.Permissions {
				if !known[name] {
					failures = append(failures, fmt.Sprintf(
						"%s (variant %q): %s asks for %q, which internal/adminaccess does not register",
						entry.Route, entry.Variant, check.Caller, name))
				}
			}
			if check.Kind != KindInvisible {
				continue
			}
			if key := InvisibleKey(entry.Route, check.Caller); invisible[key] != "" {
				allowed[key] = true
				continue
			}
			failures = append(failures, fmt.Sprintf(
				"%s (variant %q): the administration-mode check in %s accepts no registered permission: "+
					"it names a permission internal/adminaccess does not register, or its effect cannot be seen",
				entry.Route, entry.Variant, check.Caller))
		}
	}
	for key := range invisible {
		if !allowed[key] {
			failures = append(failures, fmt.Sprintf(
				"%s is declared as an invisible check, but no such check is invisible; remove the entry", key))
		}
	}
	for _, orphan := range res.Orphans {
		failures = append(failures, fmt.Sprintf(
			"%s resolved the %q mode outside any request Drive sent; the probe cannot see that check",
			orphan.Caller, orphan.Mode))
	}
	slices.Sort(failures)
	return failures
}

// Find answers the surface entries of one route, one per variant.
func (res Result) Find(method, pattern string) []Entry {
	var out []Entry
	for _, entry := range res.Surface {
		if entry.Method == method && entry.Pattern == pattern {
			out = append(out, entry)
		}
	}
	return out
}
