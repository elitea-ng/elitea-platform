package credentialbroker

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// mounted is a SecretStore whose references name a mount (Vault/OpenBao).
type mounted interface{ KVMount() string }

type acquired struct{ envelope *Envelope }

// acquire reads or exchanges the material of a permitted use and keeps only
// the catalogue's keys. Every failure is a typed denial; nothing is logged
// but codes.
func (b *Broker) acquire(ctx context.Context, req RedeemRequest, facts requestFacts, decision authzcompose.OperationDecision) (acquired, *Denial) {
	reference := facts.reference
	keys := b.cfg.Catalog.MaterialKeys(reference.ProviderType, reference.Class)
	if len(keys) == 0 {
		return acquired{}, &Denial{Code: CodeClassNotSupported, reason: "no_material_keys"}
	}
	now := b.now()
	notAfter := now.Add(MaxEnvelopeTTL)
	switch reference.Class {
	case credentialgrant.ClassStaticSecret:
		material, denial := b.readStatic(ctx, req, reference, decision, keys)
		if denial != nil {
			return acquired{}, denial
		}
		return acquired{envelope: &Envelope{Kind: KindStatic, Material: material, NotAfter: notAfter}}, nil
	case credentialgrant.ClassOAuthDelegated:
		token, denial := b.exchange(ctx, req, facts)
		if denial != nil {
			return acquired{}, denial
		}
		if len(keys) != 1 {
			return acquired{}, &Denial{Code: CodeDenied, reason: "material_shape"}
		}
		if !token.ExpiresAt.IsZero() && token.ExpiresAt.Before(notAfter) {
			notAfter = token.ExpiresAt
		}
		if !notAfter.After(now) {
			return acquired{}, &Denial{Code: CodeBackendUnavailable, reason: "material_expired"}
		}
		// A Go string cannot be zeroed; the copy below can.
		material := map[string][]byte{keys[0]: []byte(token.Token)}
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

// shapeMaterial turns a stored value into the catalogue's keys. A JSON
// object of strings gives each key its member; any other value is the one
// key of a single-key class. A missing or empty key refuses the whole
// value, and members the catalogue does not name are dropped.
func shapeMaterial(raw []byte, keys []string) (map[string][]byte, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		out := make(map[string][]byte, len(keys))
		complete := true
		for _, key := range keys {
			member, ok := object[key]
			var value string
			if !ok || json.Unmarshal(member, &value) != nil || value == "" {
				complete = false
				break
			}
			out[key] = []byte(value)
		}
		for _, member := range object {
			clear(member)
		}
		if complete {
			return out, true
		}
		for _, value := range out {
			clear(value)
		}
		if len(keys) != 1 {
			return nil, false
		}
	}
	if len(keys) != 1 || len(raw) == 0 {
		return nil, false
	}
	return map[string][]byte{keys[0]: append([]byte(nil), raw...)}, true
}

// exchange runs the on-behalf-of exchange of an oauth_delegated use. Entra
// asking for interaction records an authorization requirement and denies
// with its opaque reference.
func (b *Broker) exchange(ctx context.Context, req RedeemRequest, facts requestFacts) (obo.AccessToken, *Denial) {
	if b.cfg.Exchanger == nil || b.cfg.Custody == nil {
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "oauth_not_configured"}
	}
	r := facts.request
	if r.Authority.Kind != authzcompose.AuthorityDelegation || facts.delegation == nil {
		return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "no_delegation"}
	}
	scopes := exchangeScopes(facts.target)
	if len(scopes) == 0 {
		return obo.AccessToken{}, &Denial{Code: CodeDenied, reason: "target_has_no_audience"}
	}
	assertion, _, err := b.cfg.Custody.Load(ctx, delegationstore.LoadRequest{
		DelegationID: r.Authority.ID, TenantProjectID: r.Context.TenantProjectID, AgentBindingID: r.Agent.BindingID,
	})
	switch {
	case errors.Is(err, delegationstore.ErrDelegationNotUsable), errors.Is(err, delegationstore.ErrDelegationNotFound):
		return obo.AccessToken{}, b.authorizationRequired(ctx, req, facts, scopes, delegationstore.ReasonInteractionRequired, "")
	case err != nil:
		return obo.AccessToken{}, &Denial{Code: CodeBackendUnavailable, reason: "custody_unavailable"}
	}
	token, err := b.cfg.Exchanger.Exchange(ctx, assertion, obo.ExchangeRequest{
		DelegationID: r.Authority.ID, Scopes: scopes,
		ExecutionID: req.Run.ExecutionID, Generation: int64(req.Run.Generation),
	})
	var challenge *obo.ErrClaimsChallenge
	switch {
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

// exchangeScopes is the target's OAuth resource as an Entra v2 scope: the
// audience itself when it already names a scope, else "<audience>/.default".
func exchangeScopes(target credentialgrant.TargetBinding) []string {
	audience := strings.TrimSpace(target.Audience)
	if audience == "" {
		return nil
	}
	if strings.HasSuffix(audience, "/.default") {
		return []string{audience}
	}
	return []string{strings.TrimSuffix(audience, "/") + "/.default"}
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
		DelegationID: facts.request.Authority.ID, Scopes: scopes, Reason: reason, Claims: claims,
	})
	if err != nil || requirement.Reference == "" {
		return &Denial{Code: CodeBackendUnavailable, reason: "authorization_requirement"}
	}
	return &Denial{Code: CodeAuthorizationRequired, AuthorizationReference: requirement.Reference, reason: "authorization_required"}
}
