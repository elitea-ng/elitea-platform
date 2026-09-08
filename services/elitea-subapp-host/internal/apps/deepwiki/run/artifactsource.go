package run

// A wiki source that is an ARTIFACT FOLDER rather than a git repository.
//
// The four legacy providers all name a remote to clone. A fifth kind of
// source names a folder in the INVOKING PROJECT's artifact store: a bucket,
// and optionally a prefix inside it. The engine still indexes a directory —
// the Python side downloads the folder into the one a clone would have
// produced (elitea_deepwiki/artifact_source.py) — so everything downstream of
// the repo_config is unchanged.
//
// THE SOURCE IS SPELLED IN THE REPOSITORY STRING, `artifact://{bucket}` or
// `artifact://{bucket}/{prefix}`. The repository is the one value that
// already travels through every layer, so a scheme in it needs no new field
// anywhere, and no layer can read it as `owner/repo`.
//
// THERE IS NO HOST TO ALLOW. A clone reaches an external git host and the
// egress allowlist is what decides whether this deployment may. An artifact
// folder reaches elitea-main, over the callback the facade already minted for
// this invocation, and the project id in that grant is the only project whose
// buckets it can read. So the allowlist has nothing to check, and the bucket
// cannot be another tenant's — see CheckEgress.

import (
	"regexp"
	"strings"
)

// ArtifactProviderType is the fifth provider_type.
const ArtifactProviderType = "artifact"

// ArtifactScheme prefixes a repository string that names a folder.
const ArtifactScheme = "artifact://"

// The keys an artifact source can arrive under, prefixed or not, the way
// every other provider's configuration does.
var artifactConfigurationKeys = []string{
	"artifact_configuration",
	"toolkit_configuration_artifact_configuration",
}

// elitea-main's bucket rule (internal/api/v2/artifacts/handler.go). Checked
// here so a bad bucket is refused while the caller can still read the reason,
// rather than as an opaque 404 inside a worker.
var bucketPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// ArtifactSource is one folder: a bucket, and a prefix that may be empty.
type ArtifactSource struct {
	Bucket string
	Prefix string
}

// Repository is how this source is spelled everywhere else.
func (s ArtifactSource) Repository() string {
	if s.Prefix == "" {
		return ArtifactScheme + s.Bucket
	}
	return ArtifactScheme + s.Bucket + "/" + s.Prefix
}

// IsArtifactRepository reports whether a repository string names a folder.
func IsArtifactRepository(repository string) bool {
	return strings.HasPrefix(lower(trimSpace(repository)), ArtifactScheme)
}

// ParseArtifactRepository reads `artifact://bucket[/prefix]`.
//
// It returns false rather than an error for anything it will not accept: a
// caller that cannot parse a source has not found one, and the four legacy
// providers then run exactly as they did.
func ParseArtifactRepository(repository string) (ArtifactSource, bool) {
	if !IsArtifactRepository(repository) {
		return ArtifactSource{}, false
	}
	remainder := trimSpace(repository)[len(ArtifactScheme):]
	bucket, prefix, _ := strings.Cut(remainder, "/")
	return newArtifactSource(bucket, prefix)
}

// ArtifactSourceOf reads a source out of a merged toolkit payload: an
// `artifact_configuration` block first, then a bare `artifact://` repository.
func ArtifactSourceOf(settings map[string]any) (ArtifactSource, bool) {
	for _, key := range artifactConfigurationKeys {
		block := object(settings[key])
		if len(block) == 0 {
			continue
		}
		bucket := str(firstTruthy(block["bucket"], block["bucket_name"]))
		prefix := str(firstTruthy(block["prefix"], block["folder"]))
		if source, ok := newArtifactSource(bucket, prefix); ok {
			return source, true
		}
	}
	return ParseArtifactRepository(str(settings["repository"]))
}

func newArtifactSource(bucket, prefix string) (ArtifactSource, bool) {
	bucket = lower(trimSpace(bucket))
	if !bucketPattern.MatchString(bucket) {
		return ArtifactSource{}, false
	}
	prefix = strings.Trim(trimSpace(prefix), "/")
	if !safeKeyPrefix(prefix) {
		return ArtifactSource{}, false
	}
	return ArtifactSource{Bucket: bucket, Prefix: prefix}, true
}

// safeKeyPrefix applies elitea-main's own object-key rules
// (internal/infra/storage/ref.go): no empty, `.` or `..` segment, at most
// 1024 bytes, and no backslash.
func safeKeyPrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	if len(prefix) > 1024 || strings.ContainsAny(prefix, "\\\x00") {
		return false
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// artifactRepoConfig is the normalised repo_config an artifact source
// produces. The Python twin is elitea_deepwiki.artifact_source.repo_config_for.
func artifactRepoConfig(source ArtifactSource, branch any) RepoConfig {
	name := trimSpace(str(branch))
	if name == "" {
		name = "main"
	}
	return RepoConfig{
		ProviderType:   ArtifactProviderType,
		ProviderConfig: map[string]any{"bucket": source.Bucket, "prefix": source.Prefix},
		Repository:     source.Repository(),
		Branch:         name,
	}
}

// DisplayRepositoryFor is the repository a wiki is NAMED after.
//
// For a git source it is the repository itself. For a folder it is
// `{bucket}/{prefix}` — the scheme is dropped, because this string becomes
// the wiki id and the wiki title, and `artifact:----docs--handbook--main` is
// not a name anybody asked for.
func DisplayRepositoryFor(repoConfig map[string]any) string {
	repository := str(repoConfig["repository"])
	if repository == "" {
		repository = str(object(repoConfig["provider_config"])["repository"])
	}
	if source, ok := ParseArtifactRepository(repository); ok {
		if source.Prefix == "" {
			return source.Bucket
		}
		return source.Bucket + "/" + source.Prefix
	}
	return strings.Trim(trimSpace(repository), "/")
}
