package deepwiki

// Credential resolution for an invoke (ADR-0022 decision 6).
//
// The client sends `configuration.parameters.code_toolkit` as an INTEGER — a
// configuration id in the caller's project. The engine cannot read that: its
// repo_config normaliser expects an expanded dict carrying
// `{provider}_configuration` with the provider's own credential fields. Legacy
// elitea_core did that expansion and pushed the plaintext in the payload; this
// is the same shape with the ADR's three corrections — the egress allowlist is
// checked BEFORE any decrypt, the callback credential is short-lived and
// project-bound, and X-SECRET is gone.
//
// WHY THE EXPANSION IS A PASS-THROUGH. The platform's own configuration
// schemas (internal/api/v2/configurations/sdk_config_schemas.json) already
// spell the provider fields exactly as the engine's provider factory reads
// them — github `base_url`/`access_token`, gitlab `url`/`private_token`,
// bitbucket `url`/`username`/`password`, ado `organization_url`/`token`. So
// nothing here renames a field. Anything that looks like a mapping table below
// is the SELECTION of a provider, never a translation of its contents: a
// translation layer would be a second place for these names to live, and the
// engine is the one that decides them.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// Errors a caller of the facade can be told apart by.
var (
	// ErrToolkitNotResolvable reports a code_toolkit the caller's project does
	// not have, or one that is not a repository toolkit at all.
	ErrToolkitNotResolvable = errors.New("DeepWiki code toolkit is not resolvable")

	// ErrEgressRefused reports a repository host outside the allowlist. It is
	// returned BEFORE the vault is touched. The shared sentinel, so a caller
	// matching on it matches every facade's refusal.
	ErrEgressRefused = material.ErrEgressRefused

	// ErrCredentialsUnavailable reports a resolution that failed for a reason
	// the caller cannot fix — a vault that will not open, a database error.
	ErrCredentialsUnavailable = errors.New("DeepWiki credentials are unavailable")
)

// ConfigurationReader reads one configuration row from the caller's project.
//
// Satisfied by repos.CurrentConfigurationsRepository. Narrowed to the single
// method used, so a test double does not have to implement a CRUD surface to
// exercise a read.
type ConfigurationReader interface {
	Get(ctx context.Context, projectID, configurationID int32) (configurationapp.CurrentConfiguration, error)
}

// Unsecreter expands `{{secret.NAME}}` placeholders from the owning project's
// vault. Satisfied by storage.CurrentVaultUnsecreter — the same expansion the
// rest of the platform's configuration reads use, so a secret this facade
// pushes is the one the project actually holds.
type Unsecreter interface {
	Unsecret(ctx context.Context, configurationProjectID int32, data map[string]any) (map[string]any, error)
}

// repositoryProvider names one supported code toolkit.
//
// configurationType is the platform's own `configuration.type`;
// engineKey is the key the engine's repo_config normaliser looks for;
// hostField is the field carrying the host, which is what the egress
// allowlist is checked against.
type repositoryProvider struct {
	configurationType string
	engineKey         string
	hostField         string
	defaultHost       string
}

// The four repository toolkits the descriptor admits
// (`json_schema_extra.toolkit_types`: github, gitlab, bitbucket, ado_repos).
//
// Note the asymmetry, which is real and not a typo: the descriptor's toolkit
// TYPE is `ado_repos`, the platform's configuration type is `ado`, and the
// engine's payload key is `ado_configuration`. Three names for one thing,
// none of them ours to change.
var repositoryProviders = []repositoryProvider{
	{
		configurationType: "github",
		engineKey:         "github_configuration",
		hostField:         "base_url",
		// The engine's own default when the field is absent
		// (repo_providers/factory.py). Reproduced so the allowlist check sees
		// the host the clone will actually reach rather than an empty string.
		defaultHost: "https://api.github.com",
	},
	{
		configurationType: "gitlab",
		engineKey:         "gitlab_configuration",
		hostField:         "url",
		defaultHost:       "https://gitlab.com",
	},
	{
		configurationType: "bitbucket",
		engineKey:         "bitbucket_configuration",
		hostField:         "url",
		defaultHost:       "https://bitbucket.org",
	},
	{
		configurationType: "ado",
		engineKey:         "ado_configuration",
		hostField:         "organization_url",
		// No default: the engine reads `config.get('organization_url', '')`,
		// so an absent one is an empty host and there is nothing to allow.
		defaultHost: "",
	},
}

func providerFor(configurationType string) (repositoryProvider, bool) {
	for _, candidate := range repositoryProviders {
		if strings.EqualFold(candidate.configurationType, configurationType) {
			return candidate, true
		}
	}
	return repositoryProvider{}, false
}

// CredentialResolver expands a code_toolkit id into the payload the engine
// reads.
type CredentialResolver struct {
	configurations ConfigurationReader
	unsecreter     Unsecreter
	egress         GitEgressPolicy
}

// NewCredentialResolver refuses a half-wired resolver.
//
// A nil unsecreter would resolve configurations whose secrets are still
// `{{secret.NAME}}` placeholders — the clone would fail with a literal brace
// in the token, which reads as a bad credential rather than as a missing
// dependency. A nil egress policy would be worse: it would decrypt for any
// host at all, which is the thing decision 6 exists to prevent.
func NewCredentialResolver(
	configurations ConfigurationReader,
	unsecreter Unsecreter,
	egress GitEgressPolicy,
) (*CredentialResolver, error) {
	if configurations == nil || unsecreter == nil {
		return nil, fmt.Errorf(
			"%w: a configuration reader and an unsecreter are required",
			ErrCredentialsUnavailable)
	}
	return &CredentialResolver{
		configurations: configurations,
		unsecreter:     unsecreter,
		egress:         egress,
	}, nil
}

// ResolvedToolkit is the expanded code_toolkit.
type ResolvedToolkit struct {
	// Payload is the dict that replaces the integer code_toolkit. It carries
	// exactly one `{provider}_configuration` key plus the repository and
	// branch the caller named.
	Payload map[string]any

	// Host is the repository host that passed the allowlist. Reported so the
	// caller can log WHICH host was admitted without re-parsing the payload,
	// and so a test can assert the check saw the host it was meant to.
	Host string
}

// Resolve expands code_toolkit for one invocation.
//
// ORDER IS THE CONTRACT. The configuration row is read (it holds the host, and
// the host is not a secret), the host is checked against the allowlist, and
// only then is the vault opened. A refused host returns ErrEgressRefused
// having decrypted nothing — which is what "the egress allowlist is checked
// before any decrypt" means, and it is only true because of the order of the
// three calls below.
func (r *CredentialResolver) Resolve(
	ctx context.Context,
	projectID int32,
	toolkitID int32,
	repository string,
	branch string,
) (ResolvedToolkit, error) {
	if r == nil {
		return ResolvedToolkit{}, ErrCredentialsUnavailable
	}
	if projectID <= 0 || toolkitID <= 0 {
		return ResolvedToolkit{}, fmt.Errorf(
			"%w: project %d toolkit %d", ErrToolkitNotResolvable, projectID, toolkitID)
	}

	configuration, err := r.configurations.Get(ctx, projectID, toolkitID)
	if err != nil {
		if errors.Is(err, configurationapp.ErrCurrentConfigurationNotFound) ||
			errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest) {
			return ResolvedToolkit{}, fmt.Errorf(
				"%w: configuration %d in project %d",
				ErrToolkitNotResolvable, toolkitID, projectID)
		}
		return ResolvedToolkit{}, fmt.Errorf("%w: %s", ErrCredentialsUnavailable, err)
	}

	provider, ok := providerFor(configuration.Type)
	if !ok {
		// A configuration that exists but is, say, a Jira credential. The
		// descriptor admits four toolkit types and this is not one of them.
		return ResolvedToolkit{}, fmt.Errorf(
			"%w: configuration %d is a %q configuration, not a repository toolkit",
			ErrToolkitNotResolvable, toolkitID, configuration.Type)
	}

	// The host, read from the UNEXPANDED data. A host is not a secret, and
	// reading it here is what makes the check-before-decrypt order possible at
	// all: expanding first to find the host would decrypt every field on the
	// way to a host that may then be refused.
	host, err := providerHost(configuration.Data, provider)
	if err != nil {
		return ResolvedToolkit{}, err
	}
	if err := r.egress.Allow(host); err != nil {
		return ResolvedToolkit{}, err
	}

	expanded, err := r.unsecreter.Unsecret(ctx, projectID, configuration.Data)
	if err != nil {
		return ResolvedToolkit{}, fmt.Errorf("%w: %s", ErrCredentialsUnavailable, err)
	}
	if expanded == nil {
		expanded = map[string]any{}
	}

	payload := map[string]any{provider.engineKey: expanded}
	// repository and branch sit at the TOOLKIT level, not inside the provider
	// dict — repo_config.py reads them from the merged toolkit payload while
	// the provider dict supplies only host and credentials. Putting them in the
	// wrong half produces a clone of nothing, with no error.
	if repository != "" {
		payload["repository"] = repository
	}
	if branch != "" {
		payload["active_branch"] = branch
	}

	return ResolvedToolkit{Payload: payload, Host: host}, nil
}

// providerHost reads the host the clone will reach, from the UNEXPANDED
// row: a host is not a secret, and reading it before the vault is opened is
// what makes the check-before-decrypt order possible. The parse, and the
// refusal of a URL assembled from a secret, are material.HostFrom's.
func providerHost(data map[string]any, provider repositoryProvider) (string, error) {
	value, _ := data[provider.hostField].(string)
	return material.HostFrom(value, provider.defaultHost, provider.hostField)
}
