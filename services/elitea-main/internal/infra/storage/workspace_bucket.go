package storage

import "strings"

// The hidden workspace bucket (ADR-0034 decision 3). Each project gets at most
// one logical bucket of type WorkspaceBucketType, created on its first
// handoff, under the reserved name WorkspaceBucketName. It holds the git
// bundles of the workspace store and nothing else.
//
// Every user-facing bucket and artifact route, the worker's runtime artifact
// plane and the office-host reads refuse it with a 404, as if it did not exist.
// Two layers do that: the artifact SQL filters `bucket_type <> 'workspace'`
// (internal/db/queries/artifact_storage.sql), and the name gates below refuse
// the reserved name before any query, so a name conflict can never reveal the
// bucket either.
const (
	WorkspaceBucketType = "workspace"
	WorkspaceBucketName = "elitea-workspaces"
)

// ReservedBucketName reports whether name is reserved for a system bucket a
// user may never create, list or address. The comparison folds case and
// surrounding space: the bucket name pattern admits lowercase only, so a
// folded match is a request for the reserved bucket however it is spelled.
func ReservedBucketName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), WorkspaceBucketName)
}
