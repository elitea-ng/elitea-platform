package runtime

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseCapabilitiesBoundsAndSyntax(t *testing.T) {
	t.Parallel()

	// Exact limit (64 unique valid capabilities) succeeds.
	exactLimit := make([]string, MaxNegotiatedCapabilities)
	for i := range exactLimit {
		exactLimit[i] = fmt.Sprintf("runtime.cap_%02d.v1", i)
	}
	set, err := ParseCapabilities(exactLimit)
	if err != nil {
		t.Fatalf("exact limit failed: %v", err)
	}
	if set.Len() != MaxNegotiatedCapabilities || !set.Has("runtime.cap_00.v1") || !set.Has("runtime.cap_63.v1") {
		t.Fatalf("unexpected set: len=%d", set.Len())
	}

	// Limit+1 (65 capabilities) fails closed.
	overLimit := append(exactLimit, "runtime.cap_64.v1")
	if _, err := ParseCapabilities(overLimit); !errors.Is(err, ErrInvalidCapabilityList) {
		t.Fatalf("over limit: err = %v, want ErrInvalidCapabilityList", err)
	}

	// Exact 128-byte capability succeeds; 129-byte fails closed.
	exactName := "runtime." + strings.Repeat("a", MaxCapabilityNameBytes-len("runtime.")-len(".v1")) + ".v1"
	if len(exactName) != MaxCapabilityNameBytes {
		t.Fatalf("test bug: len = %d", len(exactName))
	}
	if _, err := ParseCapabilities([]string{exactName}); err != nil {
		t.Fatalf("exact 128-byte name failed: %v", err)
	}
	if _, err := ParseCapabilities([]string{exactName + "a"}); !errors.Is(err, ErrInvalidCapabilityList) {
		t.Fatalf("129-byte name: err = %v, want ErrInvalidCapabilityList", err)
	}

	// Malformed names and duplicates fail closed.
	badLists := map[string][]string{
		"empty entry":     {""},
		"single segment":  {"idle_compaction"},
		"uppercase":       {"runtime.IdleCompaction.v1"},
		"leading dot":     {".runtime.idle.v1"},
		"trailing dot":    {"runtime.idle.v1."},
		"consecutive dot": {"runtime..idle.v1"},
		"leading dash":    {"runtime.-idle.v1"},
		"trailing under":  {"runtime.idle_.v1"},
		"whitespace":      {"runtime.idle compaction.v1"},
		"slash":           {"runtime/idle.v1"},
		"crlf":            {"runtime.idle.v1\r\n"},
		"duplicate":       {CapabilityIdleCompaction, CapabilityIdleCompaction},
	}
	for name, list := range badLists {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseCapabilities(list); !errors.Is(err, ErrInvalidCapabilityList) {
				t.Fatalf("%s: err = %v, want ErrInvalidCapabilityList", name, err)
			}
		})
	}
}

func TestCapabilitySetMergeAndRequire(t *testing.T) {
	t.Parallel()

	set, err := ParseCapabilities([]string{CapabilityIdleCompaction, "runtime.future_feature.v2"})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := set.With(CapabilityIdleCompaction, CapabilityGovernedSecurityContext)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Len() != 3 {
		t.Fatalf("merged len = %d, want 3", merged.Len())
	}
	if err := merged.Require(CapabilityIdleCompaction, CapabilityGovernedSecurityContext); err != nil {
		t.Fatalf("require present failed: %v", err)
	}
	if err := merged.Require(CapabilityNodeRecovery); !errors.Is(err, ErrMissingRequiredCapability) {
		t.Fatalf("require absent: err = %v, want ErrMissingRequiredCapability", err)
	}
	// Mutating Strings() output must not mutate the set.
	copySlice := merged.Strings()
	copySlice[0] = "tampered.value"
	if merged.Has("tampered.value") || !merged.Has(CapabilityGovernedSecurityContext) {
		t.Fatal("Strings() aliased internal storage")
	}
}
