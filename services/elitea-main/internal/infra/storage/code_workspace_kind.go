package storage

// CodeWorkspaceKind names who owns a sandbox workspace, because the
// writable-workspace refusal depends on it (ADR-0034 decision 9).
type CodeWorkspaceKind string

const (
	// CodeWorkspaceToolkit is a toolkit Code workspace: repository content
	// at a fixed commit, acquired by Main. It stays read-only, on tmpfs,
	// with network none and its current limits.
	CodeWorkspaceToolkit CodeWorkspaceKind = "toolkit_code"
	// CodeWorkspaceStore is a cloud-executor workspace-store run: the
	// handed-off thread's tree, hydrated and published by the worker over
	// the runtime plane, in its own image and on a disk-backed volume.
	CodeWorkspaceStore CodeWorkspaceKind = "workspace_store"
)

// AdmitCodeWorkspaceMode decides whether a workspace of kind may be mounted
// in mode. Only a workspace-store run may be writable: a toolkit Code
// workspace still has no durable, quota-backed storage to write into, so its
// readwrite request keeps ErrCodeWorkspaceWritableUnavailable.
func AdmitCodeWorkspaceMode(kind CodeWorkspaceKind, mode CodeWorkspaceMode) error {
	switch {
	case mode == CodeWorkspaceRead && (kind == CodeWorkspaceToolkit || kind == CodeWorkspaceStore):
		return nil
	case mode == CodeWorkspaceReadwrite && kind == CodeWorkspaceStore:
		return nil
	case mode == CodeWorkspaceReadwrite && kind == CodeWorkspaceToolkit:
		return ErrCodeWorkspaceWritableUnavailable
	default:
		return ErrCodeWorkspaceInvalid
	}
}
