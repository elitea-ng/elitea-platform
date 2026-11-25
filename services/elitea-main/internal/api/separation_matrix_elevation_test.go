package api_test

// The elevated columns of the separation matrix against the registry
// (ADR-0059 R2.8a, issue 2522): a row the router answers `restricted` is one
// whose action table entry denies the elevation, and only such a row; a row
// whose entry flags the action is `flagged` where the gate admits it.

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// rowEntries answers the action table entries of a matrix row: its route's,
// narrowed to the selector value of its action ("action delete", "section
// email"), the way routeClass narrows them.
func rowEntries(row matrixRow) []adminaccess.Action {
	_, value, _ := strings.Cut(row.Action, " ")
	if row.Action == noAction {
		value = ""
	}
	var out []adminaccess.Action
	for _, entry := range adminaccess.ActionTable().Route(row.Method, adminaccess.RoutePattern(row.Pattern)) {
		if entry.Selector.Value == value {
			out = append(out, entry)
		}
	}
	return out
}

// elevatedWant answers what the registry says of an elevated row: whether it
// is restricted, whether it is flagged when admitted, and whether the
// registry decides it at all (false for a classified route whose outcomes
// disagree and whose handler decides after classifying, which this router,
// with no database, never reaches).
func elevatedWant(entries []adminaccess.Action) (restricted, flagged, decided bool) {
	caller := adminaccess.ElevationCaller{Elevated: true}
	first := adminaccess.DecideElevation(entries[0], caller)
	for _, entry := range entries[1:] {
		if !adminaccess.DecideElevation(entry, caller).Same(first) {
			if adminaccess.ElevationRuleInHandler[entries[0].Classifier] {
				return false, false, false
			}
			return true, false, true
		}
	}
	return first.Refusal != nil, first.Flag, true
}

func TestSeparationMatrixElevatedColumnsMatchTheRegistry(t *testing.T) {
	checked := 0
	for _, row := range readSeparationMatrix(t) {
		if row.Pending != 0 {
			continue
		}
		if len(row.Elevated) != len(separationElevations) {
			t.Errorf("%s: %d elevated columns, want %d: regenerate %s", row.key(), len(row.Elevated),
				len(separationElevations), separationMatrixFile)
			continue
		}
		entries := rowEntries(row)
		if len(entries) == 0 {
			// A read, or a write the action table pins as naming no
			// permission: no elevation value decides it.
			for i, level := range separationElevations {
				if row.Elevated[i] == classRestricted || row.Elevated[i] == classFlagged {
					t.Errorf("%s: elevated %s is %s, but no action table entry decides it",
						row.key(), level, row.Elevated[i])
				}
			}
			continue
		}
		restricted, flagged, decided := elevatedWant(entries)
		if !decided {
			continue
		}
		checked++
		for i, level := range separationElevations {
			got := row.Elevated[i]
			switch {
			case restricted && got != classRestricted:
				t.Errorf("%s: the action table denies an elevation, and elevated %s is %s", row.key(), level, got)
			case !restricted && got == classRestricted:
				t.Errorf("%s: the action table does not deny an elevation, and elevated %s is restricted", row.key(), level)
			case flagged && got == classAllowed:
				t.Errorf("%s: the action table flags the action, and elevated %s is allowed unflagged", row.key(), level)
			case !flagged && got == classFlagged:
				t.Errorf("%s: the action table does not flag the action, and elevated %s is flagged", row.key(), level)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d rows were checked against the registry; the surface has more", checked)
	}
}
