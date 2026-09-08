package deepwiki

// WHICH rewrite an invoke gets, and what a Wikis toolkit reference expands
// to (ADR-0022 decision 6).
//
// Until this file ONE rewrite served all three toolkits and it REQUIRED
// `code_toolkit`. Two consequences, both of which made a correctly written
// request fail:
//
//   - a `wiki_query` invoke — a toolkit whose configuration declares only a
//     model and an embedding, because what it reads is the project's
//     artifact bucket — was refused with "The requested code toolkit is not
//     a repository configuration in this project";
//   - a `wikis_query` invoke forwarded its bare integer id, which the
//     provider refuses on sight ("wikis_toolkit must arrive expanded; this
//     service does not resolve toolkit references, the facade does").
//
// WHY wikis_query IS TWO READS. The `wikis_toolkit` id names an APPLICATION
// toolkit row — the descriptor says so: `application: true`,
// `toolkit_types: ["wikis_Wikis", "deepwiki_Deepwiki"]` — and what the
// engine needs is that toolkit's OWN code toolkit, expanded. So the row is
// read, and the code toolkit id is taken from the ROW rather than from the
// body. That is what stops a query toolkit being a way to expand any
// configuration in the project: through toolkit 7, the only reachable code
// toolkit is the one toolkit 7 itself saved.
//
// The order CredentialResolver already guarantees is unchanged and is the
// whole point: the configuration row is read, its host is checked against
// the allowlist, and only a permitted host reaches the vault.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// Toolkit alias sets, mirroring the host's admission table
// (services/elitea-subapp-host/internal/apps/deepwiki/deepwiki.go) and the
// legacy plugin's own lists. A name that drifts here does not fail loudly —
// it falls through to the code_toolkit rewrite and refuses the caller — so a
// test compares them against the recorded conformance fixture.
// The `Wikis` family's own aliases are deliberately NOT listed: it is the
// default branch below, so listing them would be a second copy of a set
// only the provider needs to be right about.
var (
	// WikisQueryToolkits reference an existing Wikis toolkit.
	WikisQueryToolkits = []string{"wikis_query", "deepwiki_query", "DeepwikiQuery", "deepwiki-query"}
	// WikiQueryToolkits reference nothing: they read the wiki bucket.
	WikiQueryToolkits = []string{"wiki_query", "WikiQuery", "wiki-query"}
)

// AdmittedWikisToolkitTypes are the toolkit types a `wikis_toolkit` id may
// name, from the descriptor's own `json_schema_extra.toolkit_types` plus the
// two bare spellings the platform stores them under.
var AdmittedWikisToolkitTypes = []string{"wikis_Wikis", "deepwiki_Deepwiki", "wikis", "deepwiki"}

// ToolkitReader reads one application toolkit row from the caller's project.
// Satisfied by repos.CurrentToolkitsRepository, narrowed to the one method.
type ToolkitReader = material.ToolkitReader

// ErrWikisToolkitNotResolvable reports a wikis_toolkit the caller's project
// does not have, one that is not a Wikis toolkit, or one whose own
// configuration names no code toolkit.
var ErrWikisToolkitNotResolvable = errors.New("DeepWiki wikis toolkit is not resolvable")

// For chooses the rewrite for one invoke from the toolkit it addresses.
//
// An unknown toolkit name falls through to the code_toolkit rewrite rather
// than to "no rewrite": the PROVIDER refuses an unknown toolkit itself, with
// the legacy message naming every accepted alias, and duplicating that
// refusal here would be a second place for the alias list to be wrong.
func (rw *InvokeRewriter) For(toolkitName, _ string) material.Rewriter {
	switch {
	case rw == nil:
		return nil
	case slices.Contains(WikiQueryToolkits, toolkitName):
		return rw.bucketQuery.Rewrite
	case slices.Contains(WikisQueryToolkits, toolkitName):
		return rw.wikisQuery.Rewrite
	default:
		return rw.main.Rewrite
	}
}

// expandWikisToolkit expands the referenced Wikis toolkit's OWN code
// toolkit. `reference` is already that inner id: material.Owner read the
// row, checked its type and took the id out of its settings.
//
// The repository and branch come from the SAME row, not from the query
// toolkit's body: what a wikis_query invoke asks about is the wiki the
// referenced toolkit built, and letting the caller re-point it at another
// repository would answer from an index that was never generated.
func (rw *InvokeRewriter) expandWikisToolkit(
	ctx context.Context, project, reference int32,
	owner map[string]any, _ map[string]json.RawMessage,
) (any, error) {
	resolved, err := rw.credentials.Resolve(ctx, project, reference,
		material.Text(owner["repository"]),
		material.FirstText(owner, "active_branch", "branch", "base_branch"))
	if err != nil {
		return nil, err
	}
	// The shape the PROVIDER merges: the expanded object's keys land in
	// `configuration.parameters`, so `code_toolkit` is what must be in it
	// (elitea-subapp-host, run/params.go::TransformQueryRequest).
	return map[string]any{"code_toolkit": resolved.Payload}, nil
}
