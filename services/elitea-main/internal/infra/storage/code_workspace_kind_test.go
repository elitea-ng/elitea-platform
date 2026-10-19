package storage

import (
	"errors"
	"testing"
)

// ADR-0034 decision 9: only a workspace-store run may be writable; a
// toolkit Code workspace keeps the readwrite refusal.
func TestWritableWorkspacesAreAdmittedForWorkspaceStoreRunsOnly(t *testing.T) {
	cases := []struct {
		kind CodeWorkspaceKind
		mode CodeWorkspaceMode
		want error
	}{
		{CodeWorkspaceToolkit, CodeWorkspaceRead, nil},
		{CodeWorkspaceToolkit, CodeWorkspaceReadwrite, ErrCodeWorkspaceWritableUnavailable},
		{CodeWorkspaceStore, CodeWorkspaceRead, nil},
		{CodeWorkspaceStore, CodeWorkspaceReadwrite, nil},
		{CodeWorkspaceKind("other"), CodeWorkspaceRead, ErrCodeWorkspaceInvalid},
		{CodeWorkspaceStore, CodeWorkspaceMode("append"), ErrCodeWorkspaceInvalid},
	}
	for _, test := range cases {
		if got := AdmitCodeWorkspaceMode(test.kind, test.mode); !errors.Is(got, test.want) || (test.want == nil && got != nil) {
			t.Fatalf("%s/%s: got %v, want %v", test.kind, test.mode, got, test.want)
		}
	}
}
