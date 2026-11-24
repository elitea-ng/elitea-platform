package legacyrbac

import (
	"testing"
	"time"
)

// AllowTokenScopesForTest stands in a token-scope eligibility for the length
// of t: no registry row opts in as a token scope before R1.3a, so a test of
// the scoped intersection names the scopes it treats as eligible.
func AllowTokenScopesForTest(t *testing.T, names ...string) {
	t.Helper()
	previous := tokenScopeAllowed
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	tokenScopeAllowed = func(name string) bool { return allowed[name] }
	t.Cleanup(func() { tokenScopeAllowed = previous })
}

// SetClockForTest makes the resolver evaluate the elevation predicate at
// now, so a test shows that an expired elevation grants nothing with no
// sweeper running.
func SetClockForTest(r *PostgresResolver, now func() time.Time) { r.now = now }
