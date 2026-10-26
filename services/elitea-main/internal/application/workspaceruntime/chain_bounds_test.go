package workspaceruntime

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// The chain bounds Main enforces and the Rust store's writers and readers
// assume are one number each: a drift would have a writer that compacts
// too late (Main refuses its PUSH) or a reader that refuses a plan Main
// answers. The Rust constants are read from their source.
func TestChainBoundsMatchTheRustStore(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "..", "libs", "rust", "workspace-store", "src")
	for _, bound := range []struct {
		file, name string
		want       int
	}{
		{"compaction.rs", "SERVER_MAX_PUSH_CHAIN", MaxPushChain},
		{"bundle.rs", "MAX_PREREQUISITES", MaxPrerequisites},
	} {
		source, err := os.ReadFile(filepath.Join(root, bound.file))
		if err != nil {
			t.Fatalf("read %s: %v", bound.file, err)
		}
		match := regexp.MustCompile(`pub const ` + bound.name + `: usize = (\d+);`).FindSubmatch(source)
		if match == nil {
			t.Fatalf("%s does not define %s", bound.file, bound.name)
		}
		if got, _ := strconv.Atoi(string(match[1])); got != bound.want {
			t.Fatalf("Rust %s = %d; Main's is %d", bound.name, got, bound.want)
		}
	}
	if MaxPlanSegments != 1+MaxPushChain {
		t.Fatalf("MaxPlanSegments = %d; want a COMPACT and %d PUSH", MaxPlanSegments, MaxPushChain)
	}
}
