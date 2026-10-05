package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
)

// The final zero byte separates this identity from other SHA-256 uses.
// See libs/proto/elitea/runtime/v1/runtime_application_version_v1.md.
const runtimeApplicationDefinitionDigestDomain = "elitea.runtime.application-definition.v1\x00"

// runtimeApplicationDefinitionSHA256 identifies exact pre-redemption bytes.
// The caller supplies only claim-authorized and resolved identities.
// Materialized credentials and caller-selected digest fields are not inputs.
func runtimeApplicationDefinitionSHA256(
	projectID int64,
	applicationID uint64,
	versionID uint64,
	frozen json.RawMessage,
) (string, error) {
	if projectID <= 0 || projectID > math.MaxInt32 ||
		applicationID == 0 || applicationID > math.MaxInt32 ||
		versionID == 0 || versionID > math.MaxInt32 ||
		len(frozen) == 0 || len(frozen) > maxRuntimeApplicationVersionResponseBytes {
		return "", errors.New("frozen application definition is invalid")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(frozen, &object) != nil || len(object) == 0 {
		return "", errors.New("frozen application definition is invalid")
	}
	var identities [32]byte
	binary.BigEndian.PutUint64(identities[0:8], uint64(projectID))
	binary.BigEndian.PutUint64(identities[8:16], applicationID)
	binary.BigEndian.PutUint64(identities[16:24], versionID)
	binary.BigEndian.PutUint64(identities[24:32], uint64(len(frozen)))
	digest := sha256.New()
	// hash.Hash.Write always returns a nil error.
	_, _ = digest.Write([]byte(runtimeApplicationDefinitionDigestDomain))
	_, _ = digest.Write(identities[:])
	_, _ = digest.Write(frozen)
	return hex.EncodeToString(digest.Sum(nil)), nil
}
