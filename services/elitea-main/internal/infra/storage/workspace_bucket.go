package storage

import "strings"

// The hidden workspace bucket (ADR-0034 decision 3). Each project gets at most
// one logical bucket of type WorkspaceBucketType, created on its first
// handoff. Its name is WorkspaceBucketName, or WorkspaceBucketName plus a
// random suffix when a user bucket of that name already exists (created
// before the name was reserved); that user bucket keeps working.
//
// The bucket is identified by its TYPE, never by its name. Every user-facing
// bucket and artifact route, the worker's runtime artifact plane and the
// office-host reads refuse it with a 404, as if it did not exist: the
// artifact SQL filters `bucket_type <> 'workspace'`
// (internal/db/queries/artifact_storage.sql), and the gates ask
// IsWorkspaceBucket before the access list is read, so no answer differs
// from the one a missing bucket gets.
const (
	WorkspaceBucketType = "workspace"
	WorkspaceBucketName = "elitea-workspaces"
)

// ReservedBucketName reports whether a NEW user bucket may not take name: the
// reserved name and every suffixed form of it. Folding case and space, as the
// bucket name pattern admits lowercase only. Existing buckets are never
// refused by name.
func ReservedBucketName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), WorkspaceBucketName)
}
