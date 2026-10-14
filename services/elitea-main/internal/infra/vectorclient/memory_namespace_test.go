package vectorclient

import "testing"

// The Rust worker computes the same id (libs/rust/vector-index/tests/memory.rs
// pins the same four values). If either side changes, a toolkit delete would
// clean a namespace nothing writes to.
func TestMemoryNamespaceIDMatchesTheWorker(t *testing.T) {
	t.Parallel()
	for namespace, want := range map[string]string{
		"team-notes":  "86c50dff-2e9f-8a90-906f-a6d7cabb89be",
		"7":           "84ad73a1-03d1-8683-a972-c6ec7142f34c",
		"a":           "6b514a3e-18d1-81dc-aeef-bc3b735f8fce",
		"é namespace": "2e1104ce-ae0c-8b3d-97b3-c0319493e2e5",
	} {
		if got := MemoryNamespaceID(namespace); got != want {
			t.Errorf("MemoryNamespaceID(%q) = %s, want %s", namespace, got, want)
		}
	}
}
