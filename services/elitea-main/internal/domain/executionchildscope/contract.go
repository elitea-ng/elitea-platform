// Package executionchildscope defines Main's original saved-child owner relation.
package executionchildscope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const Schema = "elitea.runtime.execution-saved-child-scope.v1"
const MaxAncestors = 25
const MaxFamilyMembers = 128
const MaxDefinitionBytes = 1024 * 1024
const MaxCatalogBytes = 4 * MaxDefinitionBytes
const MaxWireBytes = 512 * 1024

var ErrDenied = errors.New("saved child scope is not authorized")
var ErrUnavailable = errors.New("saved child scope owner is unavailable")

type Purpose string

const (
	NestedHTTP     Purpose = "nested_http"
	CodeDebug      Purpose = "code_debug"
	CodeRecovery   Purpose = "code_recovery"
	CodeWorkspace  Purpose = "code_workspace"
	PlatformBroker Purpose = "platform_broker"
)

func (purpose Purpose) Valid() bool {
	switch purpose {
	case NestedHTTP, CodeDebug, CodeRecovery, CodeWorkspace, PlatformBroker:
		return true
	default:
		return false
	}
}

// Reference selects immutable Main state. It never grants authority by itself.
type Reference struct {
	ScopeID      string `json:"scope_id"`
	Revision     uint64 `json:"revision"`
	DigestSHA256 string `json:"digest_sha256"`
}

func (reference Reference) Validate() error {
	if !ValidDigest(reference.ScopeID) || reference.Revision != 1 || !ValidDigest(reference.DigestSHA256) {
		return ErrDenied
	}
	return nil
}

// OriginalFamily is registered once by the authenticated actual factory call.
// Every later consumer reads it from Main; request strings cannot recreate it.
type OriginalFamily struct {
	Kind                   string   `json:"kind"`
	ParentThreadID         string   `json:"parent_thread_id"`
	ChildThreadID          string   `json:"child_thread_id"`
	NodeID                 string   `json:"node_id"`
	OwnedNodeID            string   `json:"owned_node_id"`
	GraphStep              uint64   `json:"graph_step"`
	Ordinal                uint32   `json:"ordinal"`
	OriginalInvocationID   string   `json:"original_invocation_id"`
	OriginalCallID         string   `json:"original_call_id"`
	OriginalBatchSHA256    string   `json:"original_batch_sha256"`
	OriginalLineageWireB64 string   `json:"original_lineage_wire_b64"`
	ConfigSHA256           string   `json:"config_sha256"`
	InputSHA256            string   `json:"input_sha256"`
	WorkerSHA256           string   `json:"worker_sha256"`
	SourceSHA256           string   `json:"source_sha256"`
	BranchesSHA256         string   `json:"branches_sha256"`
	CatalogSHA256          string   `json:"catalog_sha256"`
	AdmittedThreads        []string `json:"admitted_threads"`
}

func (family OriginalFamily) Validate() error {
	if !bounded(family.ParentThreadID, 1024) || !bounded(family.ChildThreadID, 1024) || family.ParentThreadID == family.ChildThreadID ||
		!bounded(family.NodeID, 128) || !ValidDigest(family.InputSHA256) || !ValidDigest(family.CatalogSHA256) ||
		len(family.AdmittedThreads) == 0 || len(family.AdmittedThreads) > MaxFamilyMembers {
		return ErrDenied
	}
	seen := make(map[string]bool, len(family.AdmittedThreads))
	for _, thread := range family.AdmittedThreads {
		if !bounded(thread, 1024) || seen[thread] || thread == family.ParentThreadID {
			return ErrDenied
		}
		seen[thread] = true
	}
	if !seen[family.ChildThreadID] {
		return ErrDenied
	}
	switch family.Kind {
	case "agent_graph", "agent_native":
		if !bounded(family.OriginalInvocationID, 1024) || !bounded(family.OriginalCallID, 1024) || !ValidDigest(family.OriginalBatchSHA256) ||
			family.OriginalLineageWireB64 == "" || len(family.OriginalLineageWireB64) > 16384 || family.WorkerSHA256 != "" || family.SourceSHA256 != "" || family.BranchesSHA256 != "" {
			return ErrDenied
		}
	case "map_item":
		if !bounded(family.OwnedNodeID, 128) || !ValidDigest(family.ConfigSHA256) || !ValidDigest(family.WorkerSHA256) || !ValidDigest(family.SourceSHA256) || family.BranchesSHA256 != "" || family.OriginalInvocationID != "" || family.OriginalCallID != "" || family.OriginalBatchSHA256 != "" || family.OriginalLineageWireB64 != "" {
			return ErrDenied
		}
	case "parallel_branch":
		if !bounded(family.OwnedNodeID, 128) || !ValidDigest(family.ConfigSHA256) || !ValidDigest(family.BranchesSHA256) || family.WorkerSHA256 != "" || family.SourceSHA256 != "" || family.OriginalInvocationID != "" || family.OriginalCallID != "" || family.OriginalBatchSHA256 != "" || family.OriginalLineageWireB64 != "" {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	return nil
}

type SavedSelector struct {
	ApplicationID          uint64 `json:"application_id"`
	VersionID              uint64 `json:"version_id"`
	FrozenDefinitionSHA256 string `json:"frozen_definition_sha256"`
	MemberPath             string `json:"member_path"`
}

type Member struct {
	MemberPath             string           `json:"member_path"`
	ThreadID               string           `json:"thread_id"`
	ApplicationID          uint64           `json:"application_id"`
	VersionID              uint64           `json:"version_id"`
	FrozenDefinitionSHA256 string           `json:"frozen_definition_sha256"`
	YAMLSHA256             string           `json:"yaml_sha256"`
	AllowedNodeIDs         []string         `json:"allowed_node_ids"`
	SourceReference        *SourceReference `json:"source_definition"`
}

// RequestedMember must match Main-derived closure; it cannot add a member.
type RequestedMember struct {
	MemberPath             string `json:"member_path"`
	ThreadID               string `json:"thread_id"`
	FrozenDefinitionSHA256 string `json:"frozen_definition_sha256"`
}

type Registration struct {
	SchemaVersion string            `json:"schema_version"`
	Parent        *Reference        `json:"parent,omitempty"`
	Family        OriginalFamily    `json:"family"`
	Selected      SavedSelector     `json:"selected"`
	Purposes      []Purpose         `json:"purposes"`
	Members       []RequestedMember `json:"members"`
}

func (registration Registration) Validate() error {
	if registration.SchemaVersion != Schema || registration.Family.Validate() != nil || registration.Selected.ApplicationID == 0 || registration.Selected.ApplicationID > 2147483647 ||
		registration.Selected.VersionID == 0 || registration.Selected.VersionID > 2147483647 || !ValidDigest(registration.Selected.FrozenDefinitionSHA256) ||
		!validMemberPath(registration.Selected.MemberPath) || len(registration.Purposes) > 5 {
		return ErrDenied
	}
	if len(registration.Members) == 0 || len(registration.Members) > MaxFamilyMembers {
		return ErrDenied
	}
	threads := map[string]bool{}
	paths := map[string]bool{}
	for _, member := range registration.Members {
		if !validMemberPath(member.MemberPath) || !bounded(member.ThreadID, 1024) || !ValidDigest(member.FrozenDefinitionSHA256) || threads[member.ThreadID] || paths[member.MemberPath] {
			return ErrDenied
		}
		threads[member.ThreadID] = true
		paths[member.MemberPath] = true
	}
	if len(threads) != len(registration.Family.AdmittedThreads) {
		return ErrDenied
	}
	for _, thread := range registration.Family.AdmittedThreads {
		if !threads[thread] {
			return ErrDenied
		}
	}
	if registration.Parent != nil && registration.Parent.Validate() != nil {
		return ErrDenied
	}
	seen := map[Purpose]bool{}
	for _, purpose := range registration.Purposes {
		if !purpose.Valid() || seen[purpose] {
			return ErrDenied
		}
		seen[purpose] = true
	}
	return nil
}

// Paths are exact static graph member selectors, never filesystem paths. This
// matches the compiler's ASCII graph identifiers and depth-three saved closure.
func validMemberPath(path string) bool {
	if path == "" {
		return true
	}
	if len(path) > 512 {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if !ValidNodeID(part) {
			return false
		}
	}
	return true
}

func ValidNodeID(node string) bool {
	if len(node) == 0 || len(node) > 128 {
		return false
	}
	for _, ch := range node {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' && ch != '.' && ch != ':' {
			return false
		}
	}
	return true
}

// OwningDefinition is cloned immutable Main state, with no redeemed credentials.
type OwningDefinition struct {
	Reference              Reference
	SourceReference        *SourceReference
	OriginalSource         *SourceDefinition
	ThreadID               string
	MemberPath             string
	AllowedNodeIDs         []string
	ExecutionID            string
	Generation             uint64
	TenantID               string
	ResourceProjectID      int64
	ProjectionProjectID    int64
	ActorID                int64
	RootInputSHA256        string
	ApplicationID          uint64
	VersionID              uint64
	FrozenDefinitionSHA256 string
	YAMLSHA256             string
	Instructions           string
	PreRedemptionVersion   json.RawMessage
	Family                 OriginalFamily
	Ancestors              []Reference
	Purposes               []Purpose
	CanonicalWire          []byte
}

func ValidDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func Digest(bytes []byte) string { sum := sha256.Sum256(bytes); return hex.EncodeToString(sum[:]) }

func bounded(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func (definition OwningDefinition) AllowsNode(node string) bool {
	for _, allowed := range definition.AllowedNodeIDs {
		if allowed == node {
			return true
		}
	}
	return false
}
