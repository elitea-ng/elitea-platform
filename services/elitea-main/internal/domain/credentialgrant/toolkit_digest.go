package credentialgrant

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// toolkitDigestDomain separates the toolkit digest from every other sha256
// of the platform.
const toolkitDigestDomain = "elitea.credential-governance.toolkit-digest.v1\x00"

// ErrInvalidToolkitDefinition refuses a toolkit that has no type or whose
// settings are not a JSON object.
var ErrInvalidToolkitDefinition = errors.New("credential grant: the toolkit definition is invalid")

// ToolkitDigest is the AgentToolkitBinding digest of a stored toolkit
// (#1119): sha256 over the domain, the toolkit type, a NUL and the toolkit's
// stored settings as compact JSON with object keys sorted (encoding/json's
// map order). The settings are the saved row's, with configuration
// references unexpanded, so the digest changes when the toolkit is edited
// and not when a configuration it names is.
func ToolkitDigest(toolkitType string, settings map[string]any) ([32]byte, error) {
	if toolkitType == "" || settings == nil {
		return [32]byte{}, ErrInvalidToolkitDefinition
	}
	canonical, err := json.Marshal(settings)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %v", ErrInvalidToolkitDefinition, err)
	}
	material := make([]byte, 0, len(toolkitDigestDomain)+len(toolkitType)+1+len(canonical))
	material = append(material, toolkitDigestDomain...)
	material = append(material, toolkitType...)
	material = append(material, 0)
	material = append(material, canonical...)
	return sha256.Sum256(material), nil
}
