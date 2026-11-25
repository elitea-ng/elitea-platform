package adminaccess_test

import (
	"os"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// breakGlassPage is the admin page that states what an elevation cannot do.
const breakGlassPage = "../../../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx"

const (
	elevationTableStart = "{/* elevation-table:start (generated from the action table; see elevation_doc_test.go) */}"
	elevationTableEnd   = "{/* elevation-table:end */}"
)

// elevationTable is the table of every entry of the action table whose
// elevation value is not `allow`, in the table's own order.
func elevationTable() string {
	var b strings.Builder
	b.WriteString("| Action | Route | Under an elevation |\n|---|---|---|\n")
	for _, entry := range adminaccess.ActionTable().Entries() {
		var effect string
		switch entry.Elevation {
		case adminaccess.ElevationDeny:
			effect = "refused (`deny`)"
		case adminaccess.ElevationSealed:
			effect = "only a sealed `super_admin` elevation, flagged (`sealed`)"
		case adminaccess.ElevationFlag:
			effect = "allowed and flagged (`flag`)"
		default:
			continue
		}
		name := entry.Name
		if name == "" {
			name = "-"
		}
		b.WriteString("| `" + name + "` | `" + entry.Method + " " + entry.Pattern + "` | " + effect + " |\n")
	}
	return b.String()
}

// The page's table of what an elevation cannot do is the action table's
// (ADR-0059 R2.8a): a changed elevation value fails here until the page says
// it too.
func TestTheBreakGlassPageStatesTheElevationValuesOfTheActionTable(t *testing.T) {
	body, err := os.ReadFile(breakGlassPage)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, elevationTableStart)
	end := strings.Index(text, elevationTableEnd)
	if start < 0 || end < start {
		t.Fatalf("%s has no elevation table between %q and %q", breakGlassPage, elevationTableStart, elevationTableEnd)
	}
	got := strings.TrimSpace(text[start+len(elevationTableStart) : end])
	if want := strings.TrimSpace(elevationTable()); got != want {
		t.Fatalf("the elevation table of %s is not the action table's; replace it with:\n\n%s", breakGlassPage, want)
	}
}
