package executionchildscope

import (
	"bytes"
	"encoding/json"
	"io"
)

const SourceSchema = "elitea.runtime.execution-source-definition.v1"

// SourceReference is emitted by Main's capture owner and protected in original
// execution input. It is a selector, not actor/project/consumer authority.
type SourceReference struct {
	SchemaVersion          string `json:"schema_version"`
	SourceID               string `json:"source_id"`
	Revision               uint64 `json:"revision"`
	DigestSHA256           string `json:"digest_sha256"`
	SourceDefinitionSHA256 string `json:"source_definition_sha256"`
	YAMLSHA256             string `json:"yaml_sha256"`
	ApplicationID          uint64 `json:"application_id"`
	VersionID              uint64 `json:"version_id"`
	Kind                   string `json:"kind"`
}

func (ref SourceReference) Validate() error {
	if ref.SchemaVersion != SourceSchema || ref.Revision != 1 || !ValidDigest(ref.SourceID) || !ValidDigest(ref.DigestSHA256) || !ValidDigest(ref.SourceDefinitionSHA256) || !ValidDigest(ref.YAMLSHA256) {
		return ErrDenied
	}
	if ref.Kind == "saved_application" {
		if ref.ApplicationID == 0 || ref.ApplicationID > 2147483647 || ref.VersionID == 0 || ref.VersionID > 2147483647 {
			return ErrDenied
		}
	} else if ref.Kind != "ephemeral_definition" || ref.ApplicationID != 0 || ref.VersionID != 0 {
		return ErrDenied
	}
	return nil
}

type SourceWire struct {
	SchemaVersion          string `json:"schema_version"`
	SourceID               string `json:"source_id"`
	Revision               uint64 `json:"revision"`
	ResourceProjectID      int64  `json:"resource_project_id"`
	ActorID                int64  `json:"actor_id"`
	ApplicationID          uint64 `json:"application_id"`
	VersionID              uint64 `json:"version_id"`
	Kind                   string `json:"kind"`
	SourceDefinitionSHA256 string `json:"source_definition_sha256"`
	YAMLSHA256             string `json:"yaml_sha256"`
}

func NewSourceWire(project, actor int64, app, version uint64, raw []byte) (SourceWire, error) {
	if project <= 0 || project > 2147483647 || actor <= 0 || actor > 2147483647 || len(raw) == 0 || len(raw) > MaxDefinitionBytes {
		return SourceWire{}, ErrDenied
	}
	var fields map[string]json.RawMessage
	var instructions string
	if json.Unmarshal(raw, &fields) != nil || fields == nil || json.Unmarshal(fields["instructions"], &instructions) != nil {
		return SourceWire{}, ErrDenied
	}
	if len(instructions) > MaxDefinitionBytes {
		return SourceWire{}, ErrDenied
	}
	kind := "saved_application"
	if app == 0 && version == 0 {
		kind = "ephemeral_definition"
	}
	wire := SourceWire{SchemaVersion: SourceSchema, Revision: 1, ResourceProjectID: project, ActorID: actor, ApplicationID: app, VersionID: version, Kind: kind, SourceDefinitionSHA256: FrozenDefinitionDigest(project, app, version, raw), YAMLSHA256: Digest([]byte(instructions))}
	selector, _ := json.Marshal(wire)
	wire.SourceID = Digest(append([]byte("elitea.runtime.execution-source-selector.v1\x00"), selector...))
	if wire.Reference().Validate() != nil {
		return SourceWire{}, ErrDenied
	}
	return wire, nil
}
func (wire SourceWire) CanonicalBytes() []byte { raw, _ := json.Marshal(wire); return raw }
func (wire SourceWire) Reference() SourceReference {
	return SourceReference{wire.SchemaVersion, wire.SourceID, wire.Revision, Digest(wire.CanonicalBytes()), wire.SourceDefinitionSHA256, wire.YAMLSHA256, wire.ApplicationID, wire.VersionID, wire.Kind}
}
func DecodeSourceWire(raw, definition []byte, ref SourceReference, project, actor int64) (SourceDefinition, error) {
	if ref.Validate() != nil || len(raw) == 0 || len(raw) > 8192 {
		return SourceDefinition{}, ErrDenied
	}
	var stored SourceWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&stored) != nil || decoder.Decode(new(any)) != io.EOF || !bytes.Equal(raw, stored.CanonicalBytes()) {
		return SourceDefinition{}, ErrDenied
	}
	expected, err := NewSourceWire(project, actor, ref.ApplicationID, ref.VersionID, definition)
	if err != nil || expected.Reference() != ref || stored.Reference() != ref || stored.ResourceProjectID != project || stored.ActorID != actor {
		return SourceDefinition{}, ErrDenied
	}
	var fields map[string]json.RawMessage
	var instructions string
	if json.Unmarshal(definition, &fields) != nil || json.Unmarshal(fields["instructions"], &instructions) != nil {
		return SourceDefinition{}, ErrDenied
	}
	return SourceDefinition{ref, project, actor, instructions, bytes.Clone(definition), bytes.Clone(raw)}, nil
}

type SourceDefinition struct {
	Reference            SourceReference
	ResourceProjectID    int64
	ActorID              int64
	Instructions         string
	PreRedemptionVersion json.RawMessage
	CanonicalWire        []byte
}
