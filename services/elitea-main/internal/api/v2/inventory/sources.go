package inventory

// Inventory's invoke-time source credentials (ADR-0022 §6, ADR-0023 H4c I2).
//
// THE CREDENTIAL MODEL, and why the two obvious alternatives were rejected.
// The legacy plugin fetched each source toolkit itself, with
// `/api/v2/elitea_core/tool/prompt_lib/{project}/{toolkit}?expand=true` and a
// platform token it held (methods/invoke.py ~923-1060). Three ways to port it:
//
//	A — the facade expands, the provider receives material (this file).
//	B — the provider keeps a platform token scoped to reading toolkits.
//	    Rejected: there is no token scope column to hang "may read toolkits,
//	    may do nothing else" on, so the scope would be a comment.
//	C — the Go read API grows `expand=true`. Rejected: it would put a vault
//	    key behind a general-purpose read route, and the provider would still
//	    need a credential to call it.
//
// The rule under all three is the one only A keeps: THE PROVIDER MUST NEVER
// HOLD A VAULT KEY, and must never be able to ask for a credential the
// invocation did not need. Here it receives exactly the fields one source
// toolkit's SDK loader reads, for exactly the source its own toolkit's
// `sources` list already names.
//
// The mechanism — the four-step order, the bounded read, the callback grant,
// the handler — is internal/providerhost/material's. What is Inventory's is
// this file: WHICH ids may be expanded, which types, and which fields cross.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/spi"
	"github.com/go-chi/chi/v5"
)

// Env names. Spelled out, because the env-drift gate and the chart are both
// searched by the literal string.
const (
	GitAllowlistEnv     = "ELITEA_INVENTORY_GIT_ALLOWLIST"
	CallbackBaseURLEnv  = "ELITEA_INVENTORY_CALLBACK_BASE_URL"
	CallbackTokenTTLEnv = "ELITEA_INVENTORY_CALLBACK_TOKEN_TTL_MINUTES"
	SourceTypesEnv      = "ELITEA_INVENTORY_SOURCE_TYPES"
)

// ExpandingTools are the three tools whose args_schema names a source toolkit.
// The other eight read the graph ingestion already built and touch no
// credential, so they forward unrewritten.
var ExpandingTools = []string{"run_ingestion", "delta_update", "remove_source_entities"}

// GrantTools name no source but call the model: `investigate` is an agent
// over the graph that the engine answers with the toolkit's LLM, so it gets
// the callback block (a minted bearer, the gateway and the model) and
// nothing else. Without it the engine has no way to reach a model, which
// is why the Python investigate never answered (its admin client was gone
// and nothing replaced it). `smart_normalize_types` maps entity types with
// the same model, so it takes the same block.
//
// investigate's grant is also RECORDED with the toolkit's source toolkits
// (material.GrantRewriteFor): its agent calls their read-only tools through
// test_tool with that bearer, and material.SourceToolGate admits those calls
// with the chat-time execute permission rather than tool.patch.
var GrantTools = []string{"investigate", "smart_normalize_types"}

// SourceKinds is what this facade knows how to project, by toolkit type. The
// descriptor admits four (github, ado_repos, gitlab, bitbucket); two are
// wired, and the field lists come from the SDK toolkits' own config models
// (elitea_sdk/tools/{github,ado/repos}, configurations/{github,ado}.py).
var SourceKinds = map[string]material.Kind{
	"github": {
		Configuration: "github_configuration",
		HostField:     "base_url",
		// The SDK's own prefill, reproduced so the allowlist sees the host a
		// clone would actually reach rather than an empty string.
		DefaultHost: "https://api.github.com",
		Credentials: []string{
			"base_url", "access_token", "username", "password", "app_id", "app_private_key"},
		Settings: []string{"repository", "active_branch", "base_branch"},
	},
	"ado_repos": {
		Configuration: "ado_configuration",
		HostField:     "organization_url",
		// No default: ReposApiWrapper reads organization_url outright, so an
		// absent one is an empty host and there is nothing to allow.
		Credentials: []string{"organization_url", "token"},
		Settings:    []string{"project", "repository_id", "active_branch", "base_branch"},
	},
}

// Sources rewrites an Inventory invoke body.
type Sources struct{ material.SourceRewriter }

// NewSources refuses a half-wired rewriter, for the reason every composition
// root in this service has learned: it would serve perfectly well and simply
// not expand, or not check.
func NewSources(
	toolkits material.ToolkitReader,
	settings material.SettingsResolver,
	minter material.Minter,
	grants material.GrantRecorder,
	cfg SourcesConfig,
) (*Sources, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.CallbackBaseURL), "/")
	if toolkits == nil || settings == nil || minter == nil || grants == nil || base == "" {
		return nil, fmt.Errorf(
			"%w: a toolkit reader, a settings resolver, a callback minter, a grant recorder and %s are required",
			material.ErrSourceUnavailable, CallbackBaseURLEnv)
	}
	return &Sources{SourceRewriter: material.SourceRewriter{
		Provider: "inventory",
		Expander: material.Expander{
			Toolkits:     toolkits,
			Settings:     settings,
			Egress:       cfg.GitEgress,
			Kinds:        SourceKinds,
			Allowed:      cfg.SourceTypes,
			SourcesField: "sources",
			AllowlistEnv: SourceTypesEnv,
		},
		Minter:       minter,
		CallbackBase: base,
		Lifetime:     cfg.CallbackTokenTTL,
		OwnerField:   "application_id",
		SourceArg:    "toolkit_id",
		OutputField:  "source",
		Decorate:     mergeSourceConfig,
		Grants:       grants,
	}}, nil
}

// mergeSourceConfig applies the invoking toolkit's per-source configuration.
//
// PRECEDENCE IS LEGACY'S, both halves of it: the patterns in `source_configs`
// WIN over the caller's (`source_config.get("file_patterns") or
// file_patterns`), and the caller's branch wins over the stored one
// (`if source_config.get("branch") and not branch`).
func mergeSourceConfig(
	source, ownerSettings map[string]any,
	tool map[string]json.RawMessage,
	sourceID int32,
) {
	stored := material.ObjectOf(
		material.ObjectOf(ownerSettings["source_configs"])[strconv.Itoa(int(sourceID))])
	for _, key := range []string{"file_patterns", "exclude_patterns"} {
		if value := material.Text(stored[key]); value != "" {
			source[key] = value
		} else if value := material.String(tool, key); value != "" {
			source[key] = value
		}
	}
	branch := material.String(tool, "branch")
	if branch == "" {
		branch = material.Text(stored["branch"])
	}
	if branch != "" {
		source["branch"] = branch
		source["active_branch"] = branch
	}
}

// sourcesError maps a refusal to a status a caller can act on.
func sourcesError(err error) (int, string) {
	switch {
	case errors.Is(err, material.ErrSourceNotAdmitted):
		return http.StatusForbidden,
			"That toolkit is not one of this Inventory toolkit's configured sources."
	case errors.Is(err, material.ErrEgressRefused):
		return http.StatusForbidden,
			"This deployment may not clone from that repository host."
	case errors.Is(err, material.ErrSourceRefused), errors.Is(err, material.ErrRejected):
		return http.StatusBadRequest,
			"The invocation named a source this facade cannot expand."
	default:
		return http.StatusServiceUnavailable,
			"Inventory source credentials could not be resolved."
	}
}

// invoke is the handler routes.Table mounts on POST .../invoke, and nil for a
// deployment with no expansion — the shared table then forwards every tool
// plainly, and the three that name a source get the provider's own refusal
// rather than a facade that silently forwards an unexpanded id.
func (s *Sources) invoke(forward material.Forwarder, logger *slog.Logger) http.HandlerFunc {
	if s == nil {
		return nil
	}
	return material.Invocation{
		Provider: "Inventory",
		Rewrite:  s.Rewrite,
		// The grant-only tools get the callback block alone, as DeepWiki's
		// `wiki_query` does; investigate's grant is recorded besides.
		RewriteFor: s.GrantRewriteFor("investigate", GrantTools...),
		Forward:    forward,
		Path: func(r *http.Request) string {
			return spi.InvokePath(
				chi.URLParam(r, "toolkit_name"), chi.URLParam(r, "tool_name"))
		},
		Minter: s.Minter,
		Status: sourcesError,
		Logger: logger,
		Tools:  slices.Concat(ExpandingTools, GrantTools),
	}.Handler()
}
