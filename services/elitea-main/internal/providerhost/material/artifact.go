package material

// A SOURCE THAT IS A FOLDER, not a credentialed remote.
//
// Every rewrite in this package so far expands a REFERENCE: a client names a
// row id, the facade decides whether that id may be expanded, checks where a
// clone would go, and opens the vault. An artifact folder has none of those
// steps, and skipping them is the point rather than a shortcut:
//
//   - There is no credential to expand. The provider reads the folder over
//     the callback bearer this invocation is already minted (CallbackSettings
//     writes it into llm_settings), so there is no vault to open.
//   - There is no host to allow. The read goes to elitea-main, and the
//     project comes from the grant, so the ONLY buckets a caller can name are
//     the ones in the project the route already authorized. That, not the git
//     allowlist, is the boundary.
//
// What is left is validation, and it belongs here rather than in a facade
// because it is elitea-main's own rule about its own bucket and key names —
// the same rules internal/api/v2/artifacts enforces on the way in. Refusing a
// malformed source here means the caller reads why, instead of a worker
// meeting a 404 twenty minutes into a generation.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ArtifactSourceField is the toolkit-level parameter naming a folder source.
const ArtifactSourceField = "artifact_configuration"

// ArtifactScheme is how a folder source is spelled as a repository, which is
// the form every layer below the facade reads it in.
const ArtifactScheme = "artifact://"

// bucketName is internal/api/v2/artifacts/handler.go's rule.
var bucketName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// ArtifactSource is one folder: a bucket in the invoking project, and an
// optional prefix inside it.
type ArtifactSource struct {
	Bucket string
	Prefix string
}

// Repository is the string the host and the engine read the source out of.
func (s ArtifactSource) Repository() string {
	if s.Prefix == "" {
		return ArtifactScheme + s.Bucket
	}
	return ArtifactScheme + s.Bucket + "/" + s.Prefix
}

// Block is the canonical parameter the facade writes back over whatever the
// client sent, so the provider never sees an unvalidated spelling.
func (s ArtifactSource) Block() map[string]any {
	return map[string]any{"bucket": s.Bucket, "prefix": s.Prefix}
}

// ArtifactSourceIn reads a folder source out of the toolkit-level parameters.
//
// The three results are distinct and each means something different:
// (source, true, nil) is a usable source; (_, false, nil) is a request that
// names no folder at all, which is every git generation; and a non-nil error
// is a request that names one this deployment will not accept.
func ArtifactSourceIn(
	parameters map[string]json.RawMessage, refused error,
) (ArtifactSource, bool, error) {
	encoded, ok := parameters[ArtifactSourceField]
	if !ok || IsNull(encoded) || len(strings.TrimSpace(string(encoded))) == 0 {
		return ArtifactSource{}, false, nil
	}
	var block struct {
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(encoded, &block); err != nil {
		return ArtifactSource{}, false, fmt.Errorf(
			"%w: %s must be an object holding a bucket and an optional prefix",
			refused, ArtifactSourceField)
	}
	source, err := NewArtifactSource(block.Bucket, block.Prefix, refused)
	if err != nil {
		return ArtifactSource{}, false, err
	}
	return source, true, nil
}

// NewArtifactSource validates a bucket and a prefix, or says which is wrong.
func NewArtifactSource(bucket, prefix string, refused error) (ArtifactSource, error) {
	bucket = strings.ToLower(strings.TrimSpace(bucket))
	if !bucketName.MatchString(bucket) {
		return ArtifactSource{}, fmt.Errorf(
			"%w: %q is not a bucket name in this project", refused, bucket)
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if err := checkKeyPrefix(prefix, refused); err != nil {
		return ArtifactSource{}, err
	}
	return ArtifactSource{Bucket: bucket, Prefix: prefix}, nil
}

// checkKeyPrefix applies internal/infra/storage/ref.go's object-key rules to
// the folder prefix: at most 1024 bytes, and no empty, `.` or `..` segment.
func checkKeyPrefix(prefix string, refused error) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) > 1024 || strings.ContainsAny(prefix, "\\\x00") {
		return fmt.Errorf("%w: the folder is not a usable object key prefix", refused)
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf(
				"%w: the folder %q holds an empty or relative segment", refused, prefix)
		}
	}
	return nil
}
