// Package cache is the per-replica, bounded cache of governed credential
// material (issue #1121). An entry is reachable only by its full
// PartitionKey, stores the security epochs read when it was filled, and is
// served only while every current epoch equals a stored one and now is
// before not_after minus the skew. Values are zeroed when an entry leaves
// the cache. Static secrets are cached only when the run's policy revision
// allows it; OAuth access tokens until not_after minus the skew.
package cache

import (
	"crypto/sha256"
	"slices"
	"strings"
)

// PartitionKey is the full security key of a cached credential. Two
// requests that differ in any field never share an entry.
type PartitionKey struct {
	TenantProjectID       int64
	AgentBindingID        string
	SubjectPrincipalRef   string
	AuthorityID           string // mandate or delegation id
	CredentialReferenceID string
	TargetBindingID       string
	ScopeDigest           [32]byte // sorted scopes + operation class
	PolicyRevisionID      string
	Generation            int64
}

// ScopeDigest is sha256 over the sorted, de-duplicated scopes and the
// operation class, each NUL-terminated.
func ScopeDigest(scopes []string, operationClass string) [32]byte {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var b strings.Builder
	for _, scope := range sorted {
		b.WriteString(scope)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	b.WriteString(operationClass)
	b.WriteByte(0)
	return sha256.Sum256([]byte(b.String()))
}
