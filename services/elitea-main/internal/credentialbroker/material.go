package credentialbroker

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/cache"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// mounted is a SecretStore whose references name a mount (Vault/OpenBao).
type mounted interface{ KVMount() string }

type acquired struct{ envelope *Envelope }

// acquire reads or exchanges the material of a permitted use and keeps only
// the catalogue's keys. Every failure is a typed denial; nothing is logged
// but codes.
func (b *Broker) acquire(ctx context.Context, req RedeemRequest, facts requestFacts, decision authzcompose.OperationDecision,
	current []epoch.SecurityEpoch) (acquired, *Denial) {
	reference := facts.reference
	keys := b.cfg.Catalog.MaterialKeys(reference.ProviderType, reference.Class)
	if len(keys) == 0 {
		return acquired{}, &Denial{Code: CodeClassNotSupported, reason: "no_material_keys"}
	}
	now := b.now()
	notAfter := now.Add(MaxEnvelopeTTL)
	// A cached entry is reachable only after the epoch check of this
	// redemption passed, and only at the epochs it read.
	cacheable := b.cache != nil && len(current) > 0
	switch reference.Class {
	case credentialgrant.ClassStaticSecret:
		cacheable = cacheable && b.revisions.staticAllowed(ctx, facts.request.Context.PolicyRevisionID)
		var key cache.PartitionKey
		if cacheable {
			key = b.partitionKey(req, facts, keys)
			if entry, ok := b.cache.Get(key, current, now); ok {
				return acquired{envelope: fromCache(entry, KindStatic, now)}, nil
			}
		}
		material, denial := b.readStatic(ctx, req, reference, decision, keys)
		if denial != nil {
			return acquired{}, denial
		}
		if cacheable {
			b.cache.Put(key, cache.Entry{Kind: string(KindStatic), Material: material, NotAfter: now.Add(staticEntryTTL), Epochs: current})
		}
		return acquired{envelope: &Envelope{Kind: KindStatic, Material: material, NotAfter: notAfter}}, nil
	case credentialgrant.ClassOAuthDelegated:
		if len(keys) != 1 {
			return acquired{}, &Denial{Code: CodeDenied, reason: "material_shape"}
		}
		// The descriptor names who issued the consent: the user's Entra
		// delegation (SharePoint, Graph on-behalf-of) or the provider
		// itself (Jira, Confluence, Aha!). Nothing else selects the path.
		authority := b.cfg.Catalog.OAuthAuthority(reference.ProviderType, reference.Class)
		// Only on-behalf-of tokens use the replica cache. A provider OAuth
		// token is read from its grant row on every use: that row is its
		// authority, and its refresh is single-flight across replicas.
		cacheable = cacheable && authority == credentialgrant.OAuthAuthorityEntraOBO
		var key cache.PartitionKey
		if cacheable {
			key = b.partitionKey(req, facts, b.oauthScopes(facts))
			if entry, ok := b.cache.Get(key, current, now); ok {
				return acquired{envelope: fromCache(entry, KindOAuthAccess, now)}, nil
			}
		}
		var token obo.AccessToken
		var denial *Denial
		switch authority {
		case credentialgrant.OAuthAuthorityEntraOBO:
			token, denial = b.exchange(ctx, req, facts)
		case credentialgrant.OAuthAuthorityProvider:
			token, denial = b.providerToken(ctx, req, facts)
		default:
			denial = &Denial{Code: CodeClassNotSupported, reason: "oauth_authority_unknown"}
		}
		if denial != nil {
			return acquired{}, denial
		}
		if !token.ExpiresAt.IsZero() && token.ExpiresAt.Before(notAfter) {
			notAfter = token.ExpiresAt
		}
		if !notAfter.After(now) {
			return acquired{}, &Denial{Code: CodeBackendUnavailable, reason: "material_expired"}
		}
		// A Go string cannot be zeroed; the copy below can.
		material := map[string][]byte{keys[0]: []byte(token.Token)}
		if cacheable && !token.ExpiresAt.IsZero() {
			// Cached until the token's own expiry; served until that minus
			// the skew.
			b.cache.Put(key, cache.Entry{Kind: string(KindOAuthAccess), Material: material, NotAfter: token.ExpiresAt, Epochs: current})
		}
		return acquired{envelope: &Envelope{Kind: KindOAuthAccess, Material: material, NotAfter: notAfter}}, nil
	}
	return acquired{}, &Denial{Code: CodeClassNotSupported, reason: "class_not_supported"}
}

func (b *Broker) readStatic(ctx context.Context, req RedeemRequest, reference credentialgrant.CredentialReference,
	decision authzcompose.OperationDecision, keys []string) (map[string][]byte, *Denial) {
	store, ok := b.cfg.SecretStores[reference.Backend]
	if !ok {
		return nil, &Denial{Code: CodeBackendUnavailable, reason: "backend_not_configured"}
	}
	ref := secretstore.SecretRef{Backend: reference.Backend, Path: reference.Locator}
	if m, ok := store.(mounted); ok {
		ref.Mount = m.KVMount()
	}
	version, err := store.Read(ctx, secretstore.StoreContext{
		TenantProjectID: req.Run.TenantProjectID, CredentialRefID: reference.ID, DecisionID: decision.DecisionID,
		WorkloadScope: WorkloadScope, PolicyRevisionID: decision.PolicyRevisionID,
	}, ref, reference.Generation)
	if err != nil {
		reason := "backend_error"
		switch {
		case errors.Is(err, secretstore.ErrUnavailable):
			reason = "backend_unavailable"
		case errors.Is(err, secretstore.ErrNotFound):
			reason = "secret_not_found"
		case errors.Is(err, secretstore.ErrDenied):
			reason = "backend_denied"
		case errors.Is(err, secretstore.ErrInvalidRequest), errors.Is(err, secretstore.ErrMisconfigured):
			reason = "backend_misconfigured"
		}
		return nil, &Denial{Code: CodeBackendUnavailable, reason: reason}
	}
	defer version.Value.Destroy()
	material, ok := shapeMaterial(version.Value.UnsafeBytes(), keys)
	if !ok {
		return nil, &Denial{Code: CodeDenied, reason: "material_shape"}
	}
	return material, nil
}

// shapeMaterial turns a stored value into the catalogue's keys. The first
// key is required; the others are optional (a Jira PAT has a token and no
// username). A JSON object of strings gives each key its member: the first
// key missing or empty refuses the whole value, and members the catalogue
// does not name are dropped, never released. Any other value is the first
// key alone. A JSON object is never released as a raw value.
func shapeMaterial(raw []byte, keys []string) (map[string][]byte, bool) {
	if len(keys) == 0 || len(raw) == 0 {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return map[string][]byte{keys[0]: append([]byte(nil), raw...)}, true
	}
	defer func() {
		for _, member := range object {
			clear(member)
		}
	}()
	out := make(map[string][]byte, len(keys))
	for i, key := range keys {
		member, ok := object[key]
		var value string
		if ok && json.Unmarshal(member, &value) == nil && value != "" {
			out[key] = []byte(value)
			continue
		}
		if i == 0 {
			for _, v := range out {
				clear(v)
			}
			return nil, false
		}
	}
	return out, true
}

// providerToken answers the access token of the provider OAuth grant the
// use rests on (ADR-0039 decision 3: the user consented to the provider
// directly). The grant is the authority: it is the one of the run's subject
// user, for this tenant project, provider, target binding and agent
// binding. A run with no user subject has no such grant and is denied, and
// so is a decision subject that is not the run's own user (an autonomous
// mandate's approver).
func (b *Broker) providerToken(ctx context.Context, req RedeemRequest, facts requestFacts) (obo.AccessToken, *Denial) {
	if b.cfg.ProviderTokens == nil {
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "provider_oauth_not_configured"}
	}
	r := facts.request
	if facts.runSubjectUser <= 0 || r.Subject.UserID != facts.runSubjectUser {
		return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "provider_oauth_no_user_subject"}
	}
	key := provideroauth.GrantKey{
		TenantProjectID: r.Context.TenantProjectID, Provider: facts.reference.ProviderType,
		TargetBindingID: facts.reference.TargetBindingID, SubjectUserID: facts.runSubjectUser,
		AgentBindingID: r.Agent.BindingID,
	}
	token, err := b.cfg.ProviderTokens.AccessToken(ctx, key)
	switch {
	case err == nil:
	case errors.Is(err, provideroauth.ErrConsentRequired):
		return obo.AccessToken{}, b.providerConsentRequired(ctx, req, key)
	case errors.Is(err, provideroauth.ErrInvalid):
		return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "provider_oauth_key_invalid"}
	case errors.Is(err, provideroauth.ErrProviderNotConfigured):
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "provider_oauth_not_configured"}
	default:
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "provider_oauth_unavailable"}
	}
	if token.Token == "" {
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "oauth_empty_token"}
	}
	return obo.AccessToken{Token: token.Token, ExpiresAt: token.ExpiresAt}, nil
}

// providerConsentRequired answers a provider grant the user has not
// consented to. A provider whose consent a chat turn can pause for (a
// remote MCP server, #1123) is AUTHORIZATION_REQUIRED with its opaque
// reference; any other provider, or a run that is not a chat turn, is
// denied as before.
func (b *Broker) providerConsentRequired(ctx context.Context, req RedeemRequest, key provideroauth.GrantKey) *Denial {
	refused := &Denial{Code: CodeDenied, reason: "provider_consent_required"}
	if b.cfg.ProviderAuthorizations == nil {
		return refused
	}
	if parseUserID(req.Run.ActorID) != key.SubjectUserID || req.Run.ConversationUUID == "" || req.Run.ResponseMessageID == "" {
		return refused
	}
	reference, err := b.cfg.ProviderAuthorizations.Mint(ctx, provideroauth.AuthorizationRequirement{
		Key: key, ExecutionID: req.Run.ExecutionID, Generation: req.Run.Generation,
		ConversationUUID: req.Run.ConversationUUID, ResponseMessageID: req.Run.ResponseMessageID,
	})
	switch {
	case errors.Is(err, provideroauth.ErrProviderNotConfigured):
		return refused
	case err != nil || reference == "":
		return &Denial{Code: CodeBackendUnavailable, reason: "authorization_requirement"}
	}
	return &Denial{Code: CodeAuthorizationRequired, AuthorizationReference: reference, reason: "authorization_required"}
}

// exchange runs the on-behalf-of exchange of an oauth_delegated use. Entra
// asking for interaction records an authorization requirement and denies
// with its opaque reference.
func (b *Broker) exchange(ctx context.Context, req RedeemRequest, facts requestFacts) (obo.AccessToken, *Denial) {
	if b.cfg.Exchanger == nil || b.cfg.Custody == nil {
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "oauth_not_configured"}
	}
	r := facts.request
	// The delegation is the run's authority, or the one its
	// delegated_background mandate carries (buildRequest loads only that).
	if facts.delegation == nil ||
		(r.Authority.Kind != authzcompose.AuthorityDelegation && r.Authority.Kind != authzcompose.AuthorityMandate) {
		return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "no_delegation"}
	}
	delegationID := facts.delegation.ID
	scopes := b.oauthScopes(facts)
	if len(scopes) == 0 {
		return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "target_has_no_audience"}
	}
	assertion, _, err := b.cfg.Custody.Load(ctx, delegationstore.LoadRequest{
		DelegationID: delegationID, TenantProjectID: r.Context.TenantProjectID, AgentBindingID: r.Agent.BindingID,
	})
	switch {
	case errors.Is(err, delegationstore.ErrDelegationNotUsable), errors.Is(err, delegationstore.ErrDelegationNotFound):
		return obo.AccessToken{}, b.authorizationRequired(ctx, req, facts, scopes, delegationstore.ReasonInteractionRequired, "")
	case err != nil:
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "custody_unavailable"}
	}
	token, err := b.cfg.Exchanger.Exchange(ctx, assertion, obo.ExchangeRequest{
		DelegationID: delegationID, Scopes: scopes,
		ExecutionID: req.Run.ExecutionID, Generation: int64(req.Run.Generation),
	})
	var challenge *obo.ErrClaimsChallenge
	switch {
	case breakerOpen(err):
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "oauth_circuit_open"}
	case err == nil:
		if token.Token == "" {
			return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "oauth_empty_token"}
		}
		return token, nil
	case errors.As(err, &challenge):
		return obo.AccessToken{}, b.authorizationRequired(ctx, req, facts, scopes, delegationstore.ReasonClaimsChallenge, challenge.Claims)
	case errors.Is(err, obo.ErrInteractionRequired):
		return obo.AccessToken{}, b.authorizationRequired(ctx, req, facts, scopes, delegationstore.ReasonInteractionRequired, "")
	case errors.Is(err, obo.ErrUnavailable):
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "oauth_unavailable"}
	}
	return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "oauth_rejected"}
}

// oauthScopes is the exchange's scope list for the redemption's target.
func (b *Broker) oauthScopes(facts requestFacts) []string {
	resource, adapterScopes := b.cfg.Catalog.OAuthResource(facts.reference.ProviderType, facts.reference.Class)
	return exchangeScopes(facts.target, resource, adapterScopes)
}

// exchangeScopes is the Entra v2 scope list of an exchange. The resource is
// the target binding's audience, else the adapter's (Microsoft Graph for
// SharePoint). The adapter's least-privilege scopes qualify it
// ("https://graph.microsoft.com/Sites.Selected"); with none, the resource's
// "/.default" asks for what was consented. No resource is no exchange.
func exchangeScopes(target credentialgrant.TargetBinding, adapterResource string, adapterScopes []string) []string {
	resource := strings.TrimSpace(target.Audience)
	if resource == "" {
		resource = strings.TrimSpace(adapterResource)
	}
	if resource == "" {
		return nil
	}
	if strings.HasSuffix(resource, "/.default") {
		return []string{resource}
	}
	resource = strings.TrimSuffix(resource, "/")
	if len(adapterScopes) == 0 {
		return []string{resource + "/.default"}
	}
	scopes := make([]string, 0, len(adapterScopes))
	for _, scope := range adapterScopes {
		if strings.Contains(scope, "://") {
			scopes = append(scopes, scope)
			continue
		}
		scopes = append(scopes, resource+"/"+scope)
	}
	return scopes
}

func (b *Broker) authorizationRequired(ctx context.Context, req RedeemRequest, facts requestFacts, scopes []string, reason, claims string) *Denial {
	actor := parseUserID(req.Run.ActorID)
	if actor <= 0 || req.Run.ConversationUUID == "" || req.Run.ResponseMessageID == "" {
		// Only a chat turn can pause for the user; any other run is refused.
		return &Denial{Code: CodeDenied, reason: "authorization_required_unattended"}
	}
	requirement, err := b.cfg.Custody.RecordRequirement(ctx, delegationstore.Requirement{
		TenantProjectID: req.Run.TenantProjectID, ActorUserID: actor,
		ConversationUUID: req.Run.ConversationUUID, ResponseMessageID: req.Run.ResponseMessageID,
		ExecutionID: req.Run.ExecutionID, ExecutionGeneration: strconv.FormatUint(req.Run.Generation, 10),
		DelegationID: facts.delegation.ID, Scopes: scopes, Reason: reason, Claims: claims,
	})
	if err != nil || requirement.Reference == "" {
		return &Denial{Code: CodeBackendUnavailable, reason: "authorization_requirement"}
	}
	return &Denial{Code: CodeAuthorizationRequired, AuthorizationReference: requirement.Reference, reason: "authorization_required"}
}
