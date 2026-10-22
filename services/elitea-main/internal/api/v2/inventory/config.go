package inventory

// The facade's own configuration: what a deployment must say before Inventory
// may expand a source's credentials at all.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// DefaultSourceTypes are the source types a deployment allows unless
// SourceTypesEnv says otherwise. Two, not the descriptor's four: gitlab and
// bitbucket have no entry in sourceKinds, so naming them here would promise an
// expansion nobody wrote the field projection for.
var DefaultSourceTypes = []string{"github", "ado_repos"}

// DefaultCallbackTokenTTL is the bearer's lifetime when CallbackTokenTTLEnv is
// unset: eight hours, DeepWiki's (deepwiki.DefaultCallbackTokenTTL).
//
// WHY EIGHT. An ingestion is bounded by nothing on the wall clock: not this
// facade, not the sub-application host (its engine calls carry no deadline
// the caller did not set), and the Inventory engine has no CPU cap either.
// Its bearer is what the engine calls the model, the artifact store and —
// with ADR-0031 — elitea-vector with, the last of them at the end of the run,
// so the bearer must outlive the longest ingestion. Two hours was set for
// "a large repository"; a large repository with model-driven extraction runs
// past it. The same eight hours as DeepWiki keeps one rule for both engines;
// the variable still overrides it.
const DefaultCallbackTokenTTL = 8 * time.Hour

// SourcesConfig is what the rewriter needs beyond its three dependencies.
type SourcesConfig struct {
	// GitEgress is the allowlist of hosts a source may be cloned from,
	// checked BEFORE the vault is opened. Fail-closed: unset refuses
	// everything.
	GitEgress material.GitEgressPolicy
	// SourceTypes is the toolkit types this deployment allows as sources.
	SourceTypes []string
	// CallbackTokenTTL is the minted bearer's lifetime. DefaultCallbackTokenTTL
	// unless CallbackTokenTTLEnv says otherwise.
	CallbackTokenTTL time.Duration
}

// SourcesFromEnv reads it. Lenient where a default is honest and strict where
// one would hide a typo: an unparseable TTL is an error, an unset one is
// DefaultCallbackTokenTTL, and an unset allowlist is a fail-closed refusal
// rather than a silent permit.
func SourcesFromEnv(lookup func(string) (string, bool)) (SourcesConfig, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}
	cfg := SourcesConfig{
		GitEgress:        material.ParseGitEgress(get(GitAllowlistEnv), GitAllowlistEnv),
		SourceTypes:      DefaultSourceTypes,
		CallbackTokenTTL: DefaultCallbackTokenTTL,
	}
	if err := material.RetiredCallbackBaseURL(RetiredCallbackBaseURLEnv, lookup); err != nil {
		return SourcesConfig{}, err
	}
	if raw := get(SourceTypesEnv); raw != "" {
		cfg.SourceTypes = strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ' '
		})
	}
	if raw := get(CallbackTokenTTLEnv); raw != "" {
		minutes, err := strconv.Atoi(raw)
		if err != nil || minutes < 1 {
			return SourcesConfig{}, fmt.Errorf(
				"%w: %s must be a positive whole number of minutes, got %q",
				ErrInvalidRoute, CallbackTokenTTLEnv, raw)
		}
		cfg.CallbackTokenTTL = time.Duration(minutes) * time.Minute
	}
	return cfg, nil
}
