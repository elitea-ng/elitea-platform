package executionchildscope

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
)

// Wire is produced by Main after exact family registration. It contains only
// immutable selectors. Definitions are owned alongside it, never Worker state.
type Wire struct {
	SchemaVersion       string         `json:"schema_version"`
	ScopeID             string         `json:"scope_id"`
	Revision            uint64         `json:"revision"`
	ExecutionID         string         `json:"execution_id"`
	Generation          uint64         `json:"generation"`
	TenantID            string         `json:"tenant_id"`
	ResourceProjectID   int64          `json:"resource_project_id"`
	ProjectionProjectID int64          `json:"projection_project_id"`
	ActorID             int64          `json:"actor_id"`
	RootInputSHA256     string         `json:"root_input_sha256"`
	Parent              *Reference     `json:"parent,omitempty"`
	Family              OriginalFamily `json:"family"`
	Selected            SavedSelector  `json:"selected"`
	Members             []Member       `json:"members"`
	RequestedPurposes   []Purpose      `json:"requested_purposes"`
	Purposes            []Purpose      `json:"purposes"`
}

func (wire Wire) Reference() Reference {
	return Reference{wire.ScopeID, wire.Revision, Digest(wire.CanonicalBytes())}
}
func (wire Wire) CanonicalBytes() []byte {
	// The cross-language wire uses arrays even for definition-only members or
	// denied consumer grants. JSON null is not an alternate canonical image.
	if wire.Family.AdmittedThreads == nil {
		wire.Family.AdmittedThreads = []string{}
	}
	if wire.Members == nil {
		wire.Members = []Member{}
	}
	if wire.RequestedPurposes == nil {
		wire.RequestedPurposes = []Purpose{}
	}
	if wire.Purposes == nil {
		wire.Purposes = []Purpose{}
	}
	wire.Members = append([]Member{}, wire.Members...)
	for i := range wire.Members {
		if wire.Members[i].AllowedNodeIDs == nil {
			wire.Members[i].AllowedNodeIDs = []string{}
		}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil
	}
	return raw
}

// Same domain/framing as Main runtime_application_definition.go. Number tokens
// inside definition bytes are opaque; there is no cross-language JSON equality.
func FrozenDefinitionDigest(project int64, application, version uint64, raw []byte) string {
	var frame [32]byte
	binary.BigEndian.PutUint64(frame[0:8], uint64(project))
	binary.BigEndian.PutUint64(frame[8:16], application)
	binary.BigEndian.PutUint64(frame[16:24], version)
	binary.BigEndian.PutUint64(frame[24:32], uint64(len(raw)))
	hash := sha256.New()
	_, _ = hash.Write([]byte("elitea.runtime.application-definition.v1\x00"))
	_, _ = hash.Write(frame[:])
	_, _ = hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil))
}

// The slot excludes input/config/catalog/child bytes: altering those under the
// same original visit must conflict with the immutable row, not create a key.
// Native model call identity is distinct from graph visit identity.
func (wire Wire) OccurrenceID() string {
	type slot struct {
		Execution  string
		Generation uint64
		Input      string
		Parent     *Reference
		Kind       string
		Thread     string
		Node       string
		Step       uint64
		Ordinal    uint32
		Invocation string
		Batch      string
		Call       string
	}
	original := slot{Execution: wire.ExecutionID, Generation: wire.Generation, Input: wire.RootInputSHA256, Parent: wire.Parent, Kind: wire.Family.Kind, Thread: wire.Family.ParentThreadID, Ordinal: wire.Family.Ordinal}
	if wire.Family.Kind == "agent_native" {
		original.Invocation = wire.Family.OriginalInvocationID
		original.Batch = wire.Family.OriginalBatchSHA256
		original.Call = wire.Family.OriginalCallID
	} else {
		original.Node = wire.Family.NodeID
		original.Step = wire.Family.GraphStep
	}
	raw, _ := json.Marshal(original)
	return Digest(append([]byte("elitea.runtime.saved-child-occurrence.v1\x00"), raw...))
}

func (wire *Wire) Canonicalize() {
	sort.Strings(wire.Family.AdmittedThreads)
	sort.Slice(wire.Members, func(i, j int) bool { return wire.Members[i].MemberPath < wire.Members[j].MemberPath })
	sort.Slice(wire.Purposes, func(i, j int) bool { return wire.Purposes[i] < wire.Purposes[j] })
	sort.Slice(wire.RequestedPurposes, func(i, j int) bool { return wire.RequestedPurposes[i] < wire.RequestedPurposes[j] })
	wire.ScopeID = wire.OccurrenceID()
}

func DecodeWire(raw []byte) (Wire, error) {
	if len(raw) == 0 || len(raw) > MaxWireBytes {
		return Wire{}, ErrDenied
	}
	var wire Wire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || !bytes.Equal(raw, wire.CanonicalBytes()) {
		return Wire{}, ErrDenied
	}
	requested := make([]RequestedMember, len(wire.Members))
	seenRoot := false
	for i, member := range wire.Members {
		if member.SourceReference == nil || member.SourceReference.Validate() != nil || member.SourceReference.ApplicationID != member.ApplicationID || member.SourceReference.VersionID != member.VersionID || member.SourceReference.YAMLSHA256 != member.YAMLSHA256 {
			return Wire{}, ErrDenied
		}
		if member.ApplicationID == 0 || member.ApplicationID > 2147483647 || member.VersionID == 0 || member.VersionID > 2147483647 || !ValidDigest(member.YAMLSHA256) || i > 0 && wire.Members[i-1].MemberPath >= member.MemberPath {
			return Wire{}, ErrDenied
		}
		if len(member.AllowedNodeIDs) > 128 {
			return Wire{}, ErrDenied
		}
		for j, node := range member.AllowedNodeIDs {
			if !ValidNodeID(node) || j > 0 && member.AllowedNodeIDs[j-1] >= node {
				return Wire{}, ErrDenied
			}
		}
		requested[i] = RequestedMember{member.MemberPath, member.ThreadID, member.FrozenDefinitionSHA256}
		if member.MemberPath == "" {
			seenRoot = member.ThreadID == wire.Family.ChildThreadID && member.ApplicationID == wire.Selected.ApplicationID && member.VersionID == wire.Selected.VersionID && member.FrozenDefinitionSHA256 == wire.Selected.FrozenDefinitionSHA256
		}
	}
	registration := Registration{wire.SchemaVersion, wire.Parent, wire.Family, wire.Selected, wire.RequestedPurposes, requested}
	if registration.Validate() != nil || !seenRoot || wire.Revision != 1 || !executionID(wire.ExecutionID) || wire.Generation == 0 || !bounded(wire.TenantID, 128) || wire.ResourceProjectID <= 0 || wire.ResourceProjectID > 2147483647 || wire.ProjectionProjectID <= 0 || wire.ActorID <= 0 || !ValidDigest(wire.RootInputSHA256) || wire.ScopeID != wire.OccurrenceID() {
		return Wire{}, ErrDenied
	}
	for i := 1; i < len(wire.Family.AdmittedThreads); i++ {
		if wire.Family.AdmittedThreads[i-1] >= wire.Family.AdmittedThreads[i] {
			return Wire{}, ErrDenied
		}
	}
	for i := 1; i < len(wire.Purposes); i++ {
		if wire.Purposes[i-1] >= wire.Purposes[i] {
			return Wire{}, ErrDenied
		}
	}
	for i, purpose := range wire.RequestedPurposes {
		if !purpose.Valid() || i > 0 && wire.RequestedPurposes[i-1] >= purpose {
			return Wire{}, ErrDenied
		}
	}
	for _, purpose := range wire.Purposes {
		found := false
		for _, requested := range wire.RequestedPurposes {
			found = found || purpose == requested
		}
		if !found || !purpose.Valid() {
			return Wire{}, ErrDenied
		}
	}
	return wire, nil
}
func executionID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, ch := range value {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
func (wire Wire) Allows(purpose Purpose) bool {
	for _, allowed := range wire.Purposes {
		if allowed == purpose {
			return purpose.Valid()
		}
	}
	return false
}
func (wire Wire) Member(thread string) (Member, error) {
	for _, member := range wire.Members {
		if member.ThreadID == thread {
			return member, nil
		}
	}
	return Member{}, ErrDenied
}
func (wire Wire) OwningDefinition(raw, definition []byte, thread string, ancestors []Reference) (OwningDefinition, error) {
	parsed, err := DecodeWire(raw)
	if err != nil || parsed.Reference() != wire.Reference() {
		return OwningDefinition{}, ErrDenied
	}
	member, err := wire.Member(thread)
	if err != nil || len(definition) == 0 || len(definition) > MaxDefinitionBytes || FrozenDefinitionDigest(wire.ResourceProjectID, member.ApplicationID, member.VersionID, definition) != member.FrozenDefinitionSHA256 {
		return OwningDefinition{}, ErrDenied
	}
	var fields map[string]json.RawMessage
	var instructions string
	if json.Unmarshal(definition, &fields) != nil || json.Unmarshal(fields["instructions"], &instructions) != nil || Digest([]byte(instructions)) != member.YAMLSHA256 {
		return OwningDefinition{}, ErrDenied
	}
	sourceReference := *member.SourceReference
	return OwningDefinition{Reference: wire.Reference(), SourceReference: &sourceReference, ThreadID: thread, MemberPath: member.MemberPath, AllowedNodeIDs: append([]string(nil), member.AllowedNodeIDs...), ExecutionID: wire.ExecutionID, Generation: wire.Generation, TenantID: wire.TenantID, ResourceProjectID: wire.ResourceProjectID, ProjectionProjectID: wire.ProjectionProjectID, ActorID: wire.ActorID, RootInputSHA256: wire.RootInputSHA256, ApplicationID: member.ApplicationID, VersionID: member.VersionID, FrozenDefinitionSHA256: member.FrozenDefinitionSHA256, YAMLSHA256: member.YAMLSHA256, Instructions: instructions, PreRedemptionVersion: bytes.Clone(definition), Family: wire.Family, Ancestors: append([]Reference(nil), ancestors...), Purposes: append([]Purpose(nil), wire.Purposes...), CanonicalWire: bytes.Clone(raw)}, nil
}
