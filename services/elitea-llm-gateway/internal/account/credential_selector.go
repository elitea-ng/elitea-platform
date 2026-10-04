// credential_selector.go — issue #451, the second half.
//
// A model configuration row names ONE credential. The link is the object
// `data.ai_credentials`. The deleted LiteLLM mapper turned that link into
// `litellm_credential_name`, so the proxy called the provider with the one
// credential the model named.
//
// Without the link this package returns EVERY credential of the provider and
// lets bifrost/core pick one. A project with two credentials of one provider
// then gets the wrong endpoint or the wrong key, and the choice is silent.
//
// The link travels from the model resolver to this package on the request
// context. The model resolver reads the model row; this package reads the
// credential rows; the context is the only thing both sides touch.

package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
)

// LinkedCredentialReason is the rejection reason emitted when a model row names
// a credential that the caller's scopes do not hold.
const LinkedCredentialReason = "LINKED_CREDENTIAL_NOT_FOUND"

// ErrLinkedCredentialNotFound is returned when a model row names a credential
// and no credential in scope matches it.
//
// This is deliberately an error and not an empty key set. The model named ONE
// credential. If that credential is gone, disabled, or in a scope the caller
// cannot read, then dispatching with a DIFFERENT credential of the same
// provider would bill the wrong account and call the wrong endpoint. The
// request must fail, and it must say why.
var ErrLinkedCredentialNotFound = errors.New(LinkedCredentialReason)

// linkedCredentialKeyType is the private type of the context key. A pointer to
// a value of a private type cannot collide with any other package's key, and it
// cannot collide with bifrost's own reserved keys either.
type linkedCredentialKeyType struct{ name string }

// ContextKeyLinkedCredential is the request-context key under which the /llm
// handler stores the credential a model row links to. The value MUST be a
// LinkedCredential.
//
// Set it with (*schemas.BifrostContext).SetValue, exactly as the resolved
// project id is set under schemas.BifrostContextKeyVirtualKey. Absent means
// "the model named no credential", which restores the pre-#451 behaviour of
// offering every credential of the provider.
var ContextKeyLinkedCredential any = &linkedCredentialKeyType{name: "elitea-linked-credential"}

// LinkedCredential names the one credential a model row links to.
//
// ProjectID is the project whose schema holds the credential row. It is part of
// the identity because two schemas can each hold a configuration row with the
// same numeric id; a credential is only unique within its owner.
//
// ConfigID is the credential's configuration id as the account reads it, which
// is the row's uuid when it has one and its numeric id otherwise. Title is the
// row's elitea_title. ConfigID is authoritative; Title is used only when
// ConfigID is empty.
type LinkedCredential struct {
	ProjectID string
	ConfigID  string
	Title     string
	// ModelOwnerAccess is set only when the gateway resolved the link from a
	// model that the operator published from PublicProjectID. The current
	// platform treats that model as a capability: callers may run it with the
	// credential in its owning project even when the credential itself is not
	// separately published for general reuse.
	//
	// This bit is internal provenance, not request data. The account still
	// checks that ProjectID is exactly its operator-configured public project
	// before it relaxes the shared-row predicate.
	ModelOwnerAccess bool
	// DialProtocol is the AI DIAL protocol of the model row that named this
	// credential (dial_protocol.go). It is set only when the credential is an
	// ai_dial credential, and the account applies it to Azure keys only. The
	// zero value is the default protocol and changes nothing.
	//
	// It travels on the pin rather than on a key of its own because a routing
	// rule that rewrites the target clears the pin (llmproxy/policy_gate.go):
	// the protocol of the model the caller asked for must not follow the
	// request to a different model.
	DialProtocol DialProtocol
}

// empty reports whether the link names nothing at all.
func (l LinkedCredential) empty() bool {
	return l.ConfigID == "" && l.Title == ""
}

// matches reports whether c is the credential this link names.
func (l LinkedCredential) matches(c credential) bool {
	if l.ProjectID != "" && l.ProjectID != c.ownerProjectID {
		return false
	}
	if l.ConfigID != "" {
		return l.ConfigID == c.configID
	}
	return l.Title != "" && l.Title == c.name
}

// String renders the link for a log line and for an error message. It carries
// an identifier and a label only. A credential row's secret material is never
// part of this value.
func (l LinkedCredential) String() string {
	switch {
	case l.ConfigID != "" && l.ProjectID != "":
		return fmt.Sprintf("%s of project %s", l.ConfigID, l.ProjectID)
	case l.ConfigID != "":
		return l.ConfigID
	case l.ProjectID != "":
		return fmt.Sprintf("%q of project %s", l.Title, l.ProjectID)
	default:
		return fmt.Sprintf("%q", l.Title)
	}
}

// linkedCredentialFromContext reads the link the /llm handler stored. The
// second result is false when no model row named a credential.
func linkedCredentialFromContext(ctx context.Context) (LinkedCredential, bool) {
	if ctx == nil {
		return LinkedCredential{}, false
	}
	link, ok := ctx.Value(ContextKeyLinkedCredential).(LinkedCredential)
	if !ok || link.empty() {
		return LinkedCredential{}, false
	}
	return link, true
}

// selectLinkedCredential narrows creds to the one credential the model row
// named. It returns creds unchanged when no model row named a credential.
//
// It returns ErrLinkedCredentialNotFound when a link is present and no
// credential matches it. It never returns a different credential of the same
// provider: that is the silent substitution this function exists to stop.
func (a *EliteaAccount) selectLinkedCredential(
	ctx context.Context,
	projectID string,
	provider schemas.ModelProvider,
	creds []credential,
) ([]credential, error) {
	link, ok := linkedCredentialFromContext(ctx)
	if !ok {
		return creds, nil
	}
	for _, c := range creds {
		if link.matches(c) {
			return []credential{c}, nil
		}
	}
	a.logger.WarnContext(ctx, "rejected request: the model names a credential that is not in scope",
		"reason", LinkedCredentialReason,
		"project_id", projectID,
		"provider", string(provider),
		"linked_credential", link.String(),
		"candidates", len(creds),
	)
	return nil, fmt.Errorf("account: linked credential %s: %w", link, ErrLinkedCredentialNotFound)
}

// ProviderForCredentialType maps a p_{projectID}.configuration credential type
// onto the bifrost provider that serves it. The second result is false for a
// type the gateway cannot serve.
//
// It is the inverse of providerConfigTypes, which is the ONE table that says
// which credential types belong to which provider. The model resolver needs the
// same answer to derive a model's provider from its credential link (#451), so
// it reads this function rather than keeping a second copy of the table. Two
// copies would drift, and a drifted copy would send a model to a provider whose
// credentials this package never loads.
//
// The result is a schemas.ModelProvider constant, so the caller assigns it to
// the request directly. It never becomes a text prefix on a model name, which
// matters: bifrost's ParseModelString only accepts a prefix that IsKnownProvider
// admits, and a prefix it rejects is silently discarded.
func ProviderForCredentialType(credentialType string) (schemas.ModelProvider, bool) {
	if credentialType == "" {
		return "", false
	}
	for provider, types := range providerConfigTypes {
		for _, t := range types {
			if t == credentialType {
				return provider, true
			}
		}
	}
	return "", false
}

// ProviderForCredential maps one stored credential onto the Bifrost provider
// that can honour both its type and its endpoint. The platform's open_ai
// credential is also the generic OpenAI-compatible credential: when api_base
// names a non-OpenAI origin, Bifrost must use its vLLM provider because only
// that provider carries a per-key base URL.
func ProviderForCredential(credentialType, apiBase string) (schemas.ModelProvider, bool) {
	provider, ok := ProviderForCredentialType(credentialType)
	if !ok {
		return "", false
	}
	if provider == schemas.OpenAI && apiBase != "" && !isOpenAIOrigin(apiBase) {
		return schemas.VLLM, true
	}
	return provider, true
}
