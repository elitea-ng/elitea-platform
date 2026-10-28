package agentidentity

import (
	"encoding/hex"
	"errors"

	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

// ErrDefinitionUnreadable refuses to digest an empty definition.
var ErrDefinitionUnreadable = errors.New("agent identity binding: the agent version's definition is empty")

// VersionDefinitionDigest is the canonical digest of a root application
// version's frozen definition (#1462): SHA-256 over the domain
// "elitea.runtime.application-definition.v1\x00", the big-endian project,
// application and version ids and the definition's length, then the
// definition bytes. It is the framing nested child versions are digested
// with (executionchildscope.FrozenDefinitionDigest), and the
// source_definition_sha256 of a captured root source: one definition, one
// digest, whichever path computed it.
//
// definition is the version's stored definition exactly as the start
// resolver reads it (the shared application_version_details_json
// projection), before nested skills, memories or project context are added
// for one turn: those vary per run, the version's definition does not.
//
// An AgentIdentityBinding's DefinitionDigest must equal this digest of its
// application version, or admission binds no agent identity to the run.
func VersionDefinitionDigest(projectID, applicationID, versionID int64, definition []byte) ([32]byte, error) {
	var digest [32]byte
	if projectID <= 0 || applicationID <= 0 || versionID <= 0 || len(definition) == 0 {
		return digest, ErrDefinitionUnreadable
	}
	raw, err := hex.DecodeString(scope.FrozenDefinitionDigest(projectID, uint64(applicationID), uint64(versionID), definition))
	if err != nil || len(raw) != len(digest) {
		return digest, ErrDefinitionUnreadable
	}
	copy(digest[:], raw)
	return digest, nil
}
