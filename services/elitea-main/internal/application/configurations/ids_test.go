package configurations

import (
	"reflect"
	"strings"
	"testing"
)

func TestConfigurationIDsBoundsAndEmptySemantics(t *testing.T) {
	for _, raw := range []string{"", "  "} {
		ids, err := ParseCurrentConfigurationIDs(raw)
		if err != nil || len(ids) != 0 {
			t.Fatalf("empty %q: %v %v", raw, ids, err)
		}
	}
	ids, err := ParseCurrentConfigurationIDs("2, 1,2,2147483647")
	if err != nil || !reflect.DeepEqual(ids, []int32{2, 1, 2147483647}) {
		t.Fatalf("IDs: %v %v", ids, err)
	}
	for _, raw := range []string{"0", "-1", "+1", "1.1", "1,", "1,no", "2147483648", strings.Repeat("1,", 100) + "1"} {
		if _, err := ParseCurrentConfigurationIDs(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err := ParseCurrentConfigurationIDs(strings.Repeat("1,", 99) + "1"); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]int32{{0}, {-1}, make([]int32, 101)} {
		if _, err := NormalizeCurrentConfigurationIDs(ids); err == nil {
			t.Fatalf("accepted %v", ids)
		}
	}
}
