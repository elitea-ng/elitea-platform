package deepwiki

// DeepWiki's half of the git-host egress control (ADR-0022 decision 6): the
// variable an operator sets, and nothing else.
//
// The matching rules moved to internal/providerhost/material with the rest of
// the rewrite mechanics, because Inventory needs the same control over its own
// source repositories and a second copy of the `*.` subdomain rule is a second
// place for the two halves to drift from the provider's Python check. What
// stays here is the NAME: one allowlist variable per app, so a deployment can
// let DeepWiki clone from a host Inventory may not.

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/material"

// GitAllowlistEnv names the variable both halves of DeepWiki's control read.
// One variable, because two would drift and the drift would only show at
// clone time.
const GitAllowlistEnv = "ELITEA_DEEPWIKI_GIT_ALLOWLIST"

// GitEgressPolicy is the shared allowlist decision. The zero value refuses
// everything, which is the fail-closed default.
type GitEgressPolicy = material.GitEgressPolicy

// ParseGitEgressPolicy reads DeepWiki's allowlist, tagged with the variable it
// came from so a refusal names the one an operator has to set.
func ParseGitEgressPolicy(raw string) GitEgressPolicy {
	return material.ParseGitEgress(raw, GitAllowlistEnv)
}
