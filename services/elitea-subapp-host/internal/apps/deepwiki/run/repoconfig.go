package run

import (
	"log/slog"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

func trimSpace(s string) string       { return strings.TrimSpace(s) }
func hasPrefix(s, prefix string) bool { return strings.HasPrefix(s, prefix) }
func lower(s string) string           { return strings.ToLower(s) }

// The provider keys the toolkit expansion can carry, with or without the
// UI's "toolkit_configuration_" prefix.
var toolkitProviderKeys = []string{
	"github_configuration",
	"gitlab_configuration",
	"bitbucket_configuration",
	"ado_configuration",
	// The fifth, listed here and not only in the branch below so that its
	// prefixed and unprefixed spellings merge the way every other provider's
	// do. The UI and the API disagree about the prefix for all of them.
	"artifact_configuration",
}

// RepoConfig is the normalised repository configuration the engine's tools
// expect — the same keys, in the same order, as the legacy dict, because
// composed_result.json's engine_call.repo_config compares against it.
type RepoConfig struct {
	ProviderType   string         `json:"provider_type"`
	ProviderConfig map[string]any `json:"provider_config"`
	Repository     any            `json:"repository"`
	Branch         any            `json:"branch"`
	Project        any            `json:"project"`
	IsCloud        any            `json:"is_cloud"`
}

// Map is the legacy dict form, for the engine call and for comparisons.
func (r RepoConfig) Map() map[string]any {
	return map[string]any{
		"provider_type":   r.ProviderType,
		"provider_config": r.ProviderConfig,
		"repository":      r.Repository,
		"branch":          r.Branch,
		"project":         r.Project,
		"is_cloud":        r.IsCloud,
	}
}

// RepositoryString is the repository as a string, "" when unset.
func (r RepoConfig) RepositoryString() string { return str(r.Repository) }

// BranchString is the branch as a string, "" when unset.
func (r RepoConfig) BranchString() string { return str(r.Branch) }

func mergeDicts(values ...any) map[string]any {
	merged := map[string]any{}
	for _, value := range values {
		for k, v := range object(value) {
			merged[k] = v
		}
	}
	return merged
}

func configurationParametersOf(source map[string]any) map[string]any {
	if p := object(object(source["configuration"])["parameters"]); p != nil {
		return p
	}
	return map[string]any{}
}

func payloadContainsProviderKey(params map[string]any, providerKey string) bool {
	sources := []any{
		params,
		params["code_toolkit"],
		params["toolkit_configuration_code_toolkit"],
		params["toolkit_configuration_code_repository"],
		params["code_repository"],
	}
	for _, source := range sources {
		m := object(source)
		if m == nil {
			continue
		}
		if _, ok := m[providerKey]; ok {
			return true
		}
		for _, nested := range []map[string]any{object(m["settings"]), object(m["toolkit_config"]), configurationParametersOf(m)} {
			if nested == nil {
				continue
			}
			if _, ok := nested[providerKey]; ok {
				return true
			}
		}
	}
	return false
}

func mergeProviderConfigs(settings map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range settings {
		merged[k] = v
	}
	for _, providerKey := range toolkitProviderKeys {
		prefixed := "toolkit_configuration_" + providerKey
		providerConfig := mergeDicts(settings[providerKey], settings[prefixed])
		if len(providerConfig) > 0 {
			merged[providerKey] = providerConfig
			merged[prefixed] = providerConfig
		}
	}
	return merged
}

func mergeToolkitPayload(source any) map[string]any {
	m := object(source)
	if m == nil {
		return map[string]any{}
	}
	wrapper := map[string]any{}
	for k, v := range m {
		if k != "settings" && k != "toolkit_config" && k != "configuration" {
			wrapper[k] = v
		}
	}
	sources := []any{wrapper, m["toolkit_config"], configurationParametersOf(m), m["settings"]}
	merged := mergeDicts(sources...)
	for _, providerKey := range toolkitProviderKeys {
		prefixed := "toolkit_configuration_" + providerKey
		var candidates []any
		for _, candidate := range sources {
			if c := object(candidate); c != nil {
				candidates = append(candidates, c[providerKey])
			}
		}
		for _, candidate := range sources {
			if c := object(candidate); c != nil {
				candidates = append(candidates, c[prefixed])
			}
		}
		providerConfig := mergeDicts(candidates...)
		if len(providerConfig) > 0 {
			merged[providerKey] = providerConfig
			merged[prefixed] = providerConfig
		}
	}
	return merged
}

func hasAny(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func orObject(values ...any) map[string]any {
	for _, v := range values {
		if m := object(v); len(m) > 0 {
			return m
		}
	}
	return map[string]any{}
}

// ExtractRepoConfig normalises whatever shape the platform's toolkit
// expansion produced into the repo_config the engine's tools expect. Lifted
// from the legacy invoke.py at ce679f11 by way of the Python shell's
// repo_config.py: four providers, each with its own precedence chain over a
// dozen differently-prefixed keys. Copied rather than rewritten, because
// retyping that is how a provider quietly stops resolving its branch. Only
// the GitHub path is pinned by a fixture; the other three are carried on
// trust, as the Python port's README says.
func ExtractRepoConfig(params map[string]any) RepoConfig {
	codeToolkit := firstTruthy(
		params["code_toolkit"],
		params["toolkit_configuration_code_toolkit"],
		params["toolkit_configuration_code_repository"],
		params["code_repository"],
		map[string]any{},
	)
	config := RepoConfig{ProviderType: "github", ProviderConfig: map[string]any{}, Branch: "main"}

	repoSettings := map[string]any{}
	if ct := object(codeToolkit); ct != nil {
		repoSettings = mergeToolkitPayload(ct)
	}
	if len(repoSettings) == 0 && params != nil {
		repoSettings = mergeProviderConfigs(params)
	}
	s := repoSettings
	// The ARTIFACT source is tested first and returns early: it shares none of
	// the four providers' key precedence, and a payload naming one names none
	// of the others. Everything below this is the legacy chain, unchanged.
	if source, ok := ArtifactSourceOf(s); ok {
		return artifactRepoConfig(source, firstTruthy(
			s["active_branch"], s["toolkit_configuration_active_branch"],
			s["version_label"], s["branch"], "main"))
	}
	if source, ok := ArtifactSourceOf(params); ok {
		return artifactRepoConfig(source, firstTruthy(
			params["active_branch"], params["version_label"], params["branch"], "main"))
	}
	switch {
	case hasAny(s, "github_configuration", "gitlab_configuration", "bitbucket_configuration", "ado_configuration",
		"toolkit_configuration_github_configuration", "toolkit_configuration_gitlab_configuration",
		"toolkit_configuration_bitbucket_configuration", "toolkit_configuration_ado_configuration"):
		switch {
		case hasAny(s, "github_configuration", "toolkit_configuration_github_configuration"):
			config.ProviderType = "github"
			config.ProviderConfig = orObject(s["github_configuration"], s["toolkit_configuration_github_configuration"])
			config.Repository = firstTruthy(s["repository"], s["github_repository"], s["toolkit_configuration_github_repository"])
			config.Branch = firstTruthy(s["active_branch"], s["toolkit_configuration_active_branch"], s["base_branch"], s["toolkit_configuration_base_branch"], get(s, "branch", "main"))
		case hasAny(s, "gitlab_configuration", "toolkit_configuration_gitlab_configuration"):
			config.ProviderType = "gitlab"
			config.ProviderConfig = orObject(s["gitlab_configuration"], s["toolkit_configuration_gitlab_configuration"])
			config.Repository = firstTruthy(s["repository"], s["toolkit_configuration_repository"])
			config.Branch = firstTruthy(s["branch"], s["toolkit_configuration_branch"], s["active_branch"], s["toolkit_configuration_active_branch"], s["base_branch"], get(s, "toolkit_configuration_base_branch", "main"))
		case hasAny(s, "bitbucket_configuration", "toolkit_configuration_bitbucket_configuration"):
			config.ProviderType = "bitbucket"
			config.ProviderConfig = orObject(s["bitbucket_configuration"], s["toolkit_configuration_bitbucket_configuration"])
			config.Repository = firstTruthy(s["repository"], s["toolkit_configuration_repository"])
			config.Branch = firstTruthy(s["branch"], s["toolkit_configuration_branch"], s["active_branch"], s["toolkit_configuration_active_branch"], s["base_branch"], get(s, "toolkit_configuration_base_branch", "main"))
			config.Project = firstTruthy(s["project"], s["toolkit_configuration_project"])
			config.IsCloud = firstTruthy(s["cloud"], s["toolkit_configuration_cloud"])
		default: // ado
			ado := orObject(s["ado_configuration"], s["toolkit_configuration_ado_configuration"])
			config.ProviderType = "ado_repos"
			config.ProviderConfig = ado
			config.Repository = firstTruthy(s["repository_id"], s["toolkit_configuration_repository_id"], s["repository"], s["toolkit_configuration_repository"])
			config.Branch = firstTruthy(s["active_branch"], s["toolkit_configuration_active_branch"], s["base_branch"], s["toolkit_configuration_base_branch"], get(s, "branch", "main"))
			config.Project = firstTruthy(ado["project"], s["project"], s["toolkit_configuration_project"])
		}
	case len(s) > 0 || object(codeToolkit) != nil:
		// The legacy `else` on the provider-key test: settings without any
		// provider key.
		config.ProviderType = "github"
		config.ProviderConfig = orObject(s["github_configuration"])
		config.Repository = firstTruthy(s["repository"], s["github_repository"])
		config.Branch = firstTruthy(s["base_branch"], get(s, "active_branch", "main"))
	}
	if len(s) == 0 && object(codeToolkit) == nil {
		config.ProviderType = "github"
		config.ProviderConfig = orObject(params["github_configuration"])
		config.Repository = params["github_repository"]
		config.Branch = firstTruthy(params["github_base_branch"], get(params, "github_branch", "main"))
	}
	if payloadContainsProviderKey(params, "ado_configuration") && (config.ProviderType != "ado_repos" || !Truthy(config.Repository)) {
		slog.Warn("suspicious ADO repo config extraction",
			"provider_type", config.ProviderType, "repository", config.Repository, "branch", config.Branch, "project", config.Project)
	}
	return config
}

// The provider-config keys that can carry a host, in the legacy order, and
// each provider's public host when the configuration names none.
var (
	hostKeys     = []string{"base_url", "url", "api_url", "host"}
	defaultHosts = map[string]string{"github": "github.com", "gitlab": "gitlab.com", "bitbucket": "bitbucket.org", "ado_repos": "dev.azure.com"}
)

// DestinationHost mirrors what the engine's provider factory does with the
// same dict: the provider's configured base URL, else the repository when it
// is a full URL, else the provider's public default.
func DestinationHost(config RepoConfig) string {
	provider := lower(firstTruthy(config.ProviderType, "github").(string))
	for _, key := range hostKeys {
		if host := spi.HostOf(str(config.ProviderConfig[key])); host != "" {
			return host
		}
	}
	if host := spi.HostOf(config.RepositoryString()); host != "" {
		return host
	}
	return defaultHosts[provider]
}

// CheckEgress refuses a clone destination that is not on the allowlist. A
// request that names no repository at all is not a clone and passes; one
// that names one is checked, and an unresolvable host is refused too. The
// refusal is invalid_input, which the caller already knows how to read.
func CheckEgress(policy spi.EgressPolicy, params map[string]any) (string, error) {
	config := ExtractRepoConfig(params)
	// An ARTIFACT FOLDER reaches no external host, so the allowlist has
	// nothing to decide. This is NOT a hole where the check used to be: what
	// the allowlist protects against is a clone from a host the deployment
	// does not trust, and a folder is read from elitea-main over the callback
	// grant this invocation was minted with. That grant carries the project,
	// so the ONLY buckets reachable are the caller's own project's — the
	// scoping the allowlist would otherwise have to stand in for.
	if config.ProviderType == ArtifactProviderType {
		return "", nil
	}
	if len(config.ProviderConfig) == 0 && !Truthy(config.Repository) {
		return "", nil
	}
	host := DestinationHost(config)
	checked := host
	if checked == "" {
		checked = "<unresolvable>"
	}
	if err := policy.Check(checked, "clone destination"); err != nil {
		return host, err
	}
	return host, nil
}
