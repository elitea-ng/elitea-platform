package entitlements_test

import (
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
)

// TestNarrowWritesCarryTheirLegacyCapabilities pins that a narrow write or
// approve permission (ADR-0059 decision 2) is reported by the entitlement
// export with at least every capability of each legacy name it narrows. A
// role that moves from the broad name to the narrow one then shows no less
// in an access review. Reads are exempt: a read of a mapped legacy name is
// not governance-relevant by itself.
func TestNarrowWritesCarryTheirLegacyCapabilities(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, row := range adminaccess.Registry().Rows() {
		if len(row.Narrows) == 0 || row.Class == adminaccess.ClassRead {
			continue
		}
		checked++
		have := entitlements.Capabilities[row.Name]
		for _, legacy := range row.Narrows {
			for _, capability := range entitlements.Capabilities[legacy] {
				if !slices.Contains(have, capability) {
					t.Errorf("%s narrows %s, which carries %s, but maps to %v",
						row.Name, legacy, capability, have)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no narrow write row in the registry; this test checks nothing")
	}
}
