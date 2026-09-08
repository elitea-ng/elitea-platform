package deepwiki

// Rewriting an invoke body before it crosses the hop (ADR-0022 decision 6).
//
// The client sends references; the provider receives material. DeepWiki's
// three toolkits reference three different things, and each gets its own
// rewrite (wikis.go names them):
//
//	Wikis        code_toolkit: 42   →  code_toolkit: {github_configuration: …}
//	wikis_query  wikis_toolkit: 7   →  wikis_toolkit: {code_toolkit: {…}}
//	wiki_query   (nothing)          →  llm_settings only; it reads a bucket
//
// The llm_settings substitution is UNCONDITIONAL on all three, and so is the
// refusal of a client-supplied expansion: those are the fields that carry
// credentials, and a facade that accepted them would let any caller push a
// secret of their choosing to a service that then uses it to clone and to
// call back. What the client may choose is which configuration in their own
// project to use, and the platform expands it under the caller's own
// project membership.
//
// THE MECHANICS ARE NOT HERE. The bounded read, the
// `configuration.parameters` split, the owning-toolkit read, the callback
// block and its tool-level lift, the git-host egress check and the invoke
// handler that revokes a refused invocation's grant all live in
// internal/providerhost/material, because Inventory does the same things to
// different field names. What is left in this file is the two names
// DeepWiki rewrites and the statuses its refusals map to.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// ErrInvokeRejected reports a body the facade will not forward. It is the
// shared sentinel: a caller matching on it matches every facade's refusal.
var ErrInvokeRejected = material.ErrRejected

// CallbackMinter mints the short-lived, project-bound bearer the provider
// calls back with.
type CallbackMinter = material.Minter

// CallbackGrant is one minted bearer.
type CallbackGrant = material.Grant

// grantDescription is the name a minted callback token is filed under. It
// is the legacy string, lower-cased, and a test pins it.
const grantDescription = "deepwiki"

// InvokeRewriter turns a reference-carrying body into a material-carrying
// one — one rewrite per toolkit family, chosen by For (wikis.go).
type InvokeRewriter struct {
	credentials *CredentialResolver
	main        material.ReferenceRewriter
	wikisQuery  material.ReferenceRewriter
	bucketQuery material.ReferenceRewriter
}

// NewInvokeRewriter refuses one that cannot do its job.
//
// toolkits, and only toolkits, may be nil: it is needed for the
// `wikis_query` rewrite alone, and a deployment that cannot read toolkit
// rows still serves `Wikis` and `wiki_query` — it refuses `wikis_query`
// with a message saying so rather than failing to boot.
func NewInvokeRewriter(
	credentials *CredentialResolver,
	toolkits ToolkitReader,
	minter CallbackMinter,
	callbackBase string,
	lifetime time.Duration,
) (*InvokeRewriter, error) {
	if credentials == nil || minter == nil {
		return nil, fmt.Errorf(
			"%w: a credential resolver and a callback minter are required",
			ErrCredentialsUnavailable)
	}
	base := strings.TrimRight(strings.TrimSpace(callbackBase), "/")
	if base == "" {
		return nil, fmt.Errorf(
			"%w: %s is required — the provider cannot call back to an unnamed origin",
			ErrCredentialsUnavailable, CallbackBaseURLEnv)
	}
	rewriter := &InvokeRewriter{credentials: credentials}
	shared := material.ReferenceRewriter{
		Provider: grantDescription, Minter: minter, CallbackBase: base, Lifetime: lifetime,
		Refused: ErrToolkitNotResolvable, Unavailable: ErrCredentialsUnavailable,
	}
	rewriter.main = shared
	rewriter.main.Field, rewriter.main.Output = "code_toolkit", "code_toolkit"
	rewriter.main.Expand = rewriter.expandCodeToolkit
	// A `Wikis` generation may name a FOLDER instead of a repository toolkit:
	// a bucket in this project, read over the callback bearer minted below.
	// Only this toolkit — a wiki query reads the wiki bucket, which is not a
	// source at all.
	rewriter.main.FolderSource = true

	rewriter.wikisQuery = shared
	rewriter.wikisQuery.Field, rewriter.wikisQuery.Output = "wikis_toolkit", "wikis_toolkit"
	// The pre-rename spelling, which toolkit configurations created before
	// Deepwiki → Wikis still carry. The host accepts both, so this must.
	rewriter.wikisQuery.Aliases = []string{"deepwiki_toolkit"}
	rewriter.wikisQuery.Refused = ErrWikisToolkitNotResolvable
	rewriter.wikisQuery.Owner = &material.Owner{
		Toolkits: toolkits, Types: AdmittedWikisToolkitTypes,
		Keys: []string{"code_toolkit", "toolkit_configuration_code_toolkit"},
	}
	rewriter.wikisQuery.Expand = rewriter.expandWikisToolkit

	// No Field: a wiki_query toolkit names no reference at all.
	rewriter.bucketQuery = shared
	return rewriter, nil
}

// Rewrite is the `Wikis` family's rewrite, and the fallback For returns for
// a toolkit name this facade does not know.
func (rw *InvokeRewriter) Rewrite(
	ctx context.Context, body io.Reader, projectID, userID int64,
) ([]byte, CallbackGrant, error) {
	return rw.main.Rewrite(ctx, body, projectID, userID)
}

// expandCodeToolkit is the `Wikis` toolkit's expansion: the id the CLIENT
// named, resolved under the caller's own project membership, with the
// repository and branch the body carries.
func (rw *InvokeRewriter) expandCodeToolkit(
	ctx context.Context, project, reference int32,
	_ map[string]any, parameters map[string]json.RawMessage,
) (any, error) {
	resolved, err := rw.credentials.Resolve(ctx, project, reference,
		material.String(parameters, "repository"),
		material.FirstString(parameters, "active_branch", "branch", "base_branch"))
	if err != nil {
		return nil, err
	}
	return resolved.Payload, nil
}

// invokeError maps a rewrite failure to a status a caller can act on.
func invokeError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrEgressRefused):
		// The caller named a repository this deployment may not clone from.
		// Their input, their fix — but the message names the variable rather
		// than the allowlist's contents, which is an operator's business.
		return http.StatusForbidden,
			"This deployment may not clone from that repository host."
	case errors.Is(err, ErrWikisToolkitNotResolvable):
		return http.StatusBadRequest,
			"The requested Wikis toolkit is not usable from this query toolkit."
	case errors.Is(err, ErrToolkitNotResolvable):
		return http.StatusBadRequest,
			"The requested code toolkit is not a repository configuration in this project."
	case errors.Is(err, ErrInvokeRejected):
		return http.StatusBadRequest, "The invocation request could not be read."
	default:
		return http.StatusServiceUnavailable,
			"DeepWiki credentials could not be resolved."
	}
}
