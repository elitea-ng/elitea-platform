package admin

import (
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// TestEveryConfigurationNameOpensAConsoleItem: the Configuration routes admit
// runtime.plugins and every narrow name a section declares. Each must open at
// least one item of the console item table (ADR-0059 R3.1a), so a holder of
// any of them finds the section in the capability document.
func TestEveryConfigurationNameOpensAConsoleItem(t *testing.T) {
	t.Parallel()
	mapped := map[string]bool{}
	for _, item := range adminaccess.ConsoleItems() {
		if !item.Available {
			continue
		}
		for _, name := range item.Permissions {
			mapped[name] = true
		}
	}
	names := configurationRoutePermissions()
	slices.Sort(names)
	for _, name := range names {
		if !mapped[name] {
			t.Errorf("the Configuration routes admit %q, but no available console item maps it "+
				"(internal/adminaccess/console.go)", name)
		}
	}
}
