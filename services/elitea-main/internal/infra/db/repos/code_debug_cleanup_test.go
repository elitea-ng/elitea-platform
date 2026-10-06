package repos

import (
	"strings"
	"testing"
)

func TestCodeDebugCleanupOwnsOnlyExactReservedProjectBucketKey(t *testing.T) {
	key := strings.Repeat("a", 64) + ".json"
	if _, err := codeDebugCleanupRef(7, key); err != nil {
		t.Fatal(err)
	}
	for _, project := range []int64{-1, 0, 2147483648} {
		if _, err := codeDebugCleanupRef(project, key); err == nil {
			t.Fatal("invalid cleanup project accepted")
		}
	}
	for _, name := range []string{"../" + key, "other/" + key, strings.Repeat("A", 64) + ".json", strings.Repeat("a", 64) + ".py", strings.Repeat("λ", 32) + ".json", ""} {
		if _, err := codeDebugCleanupRef(7, name); err == nil {
			t.Fatal("non-reserved cleanup key accepted", name)
		}
	}
}
