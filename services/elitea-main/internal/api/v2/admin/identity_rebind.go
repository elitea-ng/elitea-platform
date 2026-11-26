package admin

// `POST /admin/identity_providers/administration/{key}/rebind`: move the
// accounts linked under one provider and issuer onto a provider's CURRENT
// issuer (issue 2558).
//
// # Why there is an explicit operation
//
// A sign-in matches (provider key, issuer, subject) (identityrepo.ProviderBinding),
// so after a super_admin changes a provider's issuer, every account linked under
// the old one is refused with 409. Two ways out were weighed:
//
//   - re-link on the next sign-in when the address is verified. Refused: the
//     address is a claim of the very identity provider whose trust is in
//     question, so this is the takeover again, with one extra claim;
//   - an explicit, audited re-bind that only a super_admin can run. This one.
//
// The super_admin states which binding the accounts leave: a provider key
// (default: this provider) and an issuer, or `from_unbound` for the links of
// this protocol that have no binding at all (a protocol with no enabled stored
// provider at the upgrade to shared migration 0258). The links move onto this
// provider's key and current issuer, and the old binding is removed, so a
// retired issuer stops matching. One security audit event
// (ai.elitea.security.v1.identity.link.changed, action identity.binding.rebound)
// is written in the same transaction, and the request's audit row is annotated.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ActionIdentityBindingRebound is the security audit action of a re-bind.
const ActionIdentityBindingRebound = "identity.binding.rebound"

// identityRebindBody is the wire shape of a re-bind.
type identityRebindBody struct {
	// FromProviderKey is the key the links are bound to now. Empty means this
	// provider. `@environment` names the OIDC_ISSUER_URL provider.
	FromProviderKey string `json:"from_provider_key"`
	// FromIssuer is the issuer (or IdP entity id) they are bound to now.
	FromIssuer string `json:"from_issuer"`
	// FromUnbound selects the links of this protocol with no binding at all,
	// instead of FromProviderKey and FromIssuer.
	FromUnbound bool `json:"from_unbound"`
}

// IdentityProviderRebind answers the re-bind route.
func (h *Handler) IdentityProviderRebind(w http.ResponseWriter, r *http.Request) {
	if !h.identityProvidersReady(w) {
		return
	}
	key := identityproviders.NormalizeKey(chi.URLParam(r, "key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid provider key"})
		return
	}
	if !h.requireIdentityTrust(w, r, trustGate{
		code:          CodeIdentityRebindSuperAdminRequired,
		message:       "only a super admin can re-bind accounts to an identity provider",
		refusedAction: AuditIdentityRebindRefused,
		entityType:    "identity_provider",
		entityName:    key,
	}) {
		return
	}
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "identity provider configuration is not available on this deployment"})
		return
	}

	request, ok := decodeIdentityRebind(w, r, key)
	if !ok {
		return
	}
	provider, err := h.identityProviders.Lookup(r.Context(), key)
	switch {
	case errors.Is(err, identityproviders.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such identity provider"})
		return
	case err != nil:
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the identity provider definitions could not be read"})
		return
	}
	request.Target = provider.Binding()
	request.Prefix = identityrepo.OIDCProviderRefPrefix
	if provider.Kind == identityproviders.KindSAML {
		request.Prefix = identityrepo.SAMLProviderRefPrefix
	}

	rebound, ok := h.applyIdentityRebind(w, r, request)
	if !ok {
		return
	}
	annotateTrustChange(r, AuditIdentityRebound, "identity_provider", key, nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_key": provider.Key,
		"issuer":       request.Target.Issuer,
		"rebound":      rebound,
	})
}

// The bounds of a re-bind source. A provider key fits the audit reason code
// `from_provider:<key>` (128 characters in all); an issuer is a URL or a URN.
const (
	maxRebindProviderKeyLength = 64
	maxRebindIssuerLength      = 2048
)

// decodeIdentityRebind reads the body into a request with its source set.
func decodeIdentityRebind(w http.ResponseWriter, r *http.Request, key string) (identityrepo.RebindRequest, bool) {
	var body identityRebindBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return identityrepo.RebindRequest{}, false
	}
	issuer := strings.TrimSpace(body.FromIssuer)
	fromKey := strings.TrimSpace(body.FromProviderKey)
	if body.FromUnbound {
		if issuer != "" || fromKey != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "from_unbound excludes from_provider_key and from_issuer",
			})
			return identityrepo.RebindRequest{}, false
		}
		return identityrepo.RebindRequest{}, true
	}
	if issuer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "name the issuer the accounts are bound to now (from_issuer), or set from_unbound",
			"field": "from_issuer",
		})
		return identityrepo.RebindRequest{}, false
	}
	if len(issuer) > maxRebindIssuerLength {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "from_issuer is too long", "field": "from_issuer",
		})
		return identityrepo.RebindRequest{}, false
	}
	switch fromKey {
	case "":
		fromKey = key
	case identityrepo.EnvironmentProviderKey:
	default:
		// Checked HERE, not left to the audit reason code: that check runs
		// after the links have moved, so a bad key would surface as a 503 and a
		// rolled-back re-bind instead of a 400 that names the field.
		normalized := identityproviders.NormalizeKey(fromKey)
		if normalized == "" || len(normalized) > maxRebindProviderKeyLength {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "from_provider_key must be a provider key or @environment",
				"field": "from_provider_key",
			})
			return identityrepo.RebindRequest{}, false
		}
		fromKey = normalized
	}
	return identityrepo.RebindRequest{From: &identityrepo.ProviderBinding{Key: fromKey, Issuer: issuer}}, true
}

// applyIdentityRebind moves the links and writes the security audit event in
// one transaction.
func (h *Handler) applyIdentityRebind(
	w http.ResponseWriter, r *http.Request, request identityrepo.RebindRequest,
) (int64, bool) {
	unavailable := func(err error) (int64, bool) {
		slog.ErrorContext(r.Context(), "admin: identity re-bind failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the accounts could not be re-bound"})
		return 0, false
	}
	if !request.Target.Valid() {
		writeJSON(w, http.StatusConflict,
			map[string]any{"error": "this identity provider has no issuer to bind accounts to"})
		return 0, false
	}
	principal, _ := auth.UserFromContext(r.Context())
	operator, ok := principal.OwningUserID()
	if !ok {
		return unavailable(errors.New("the caller has no user id"))
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rebound, err := identityrepo.RebindLinks(ctx, tx, request)
	if errors.Is(err, identityrepo.ErrRebindSameBinding) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the accounts are already bound to this provider and issuer",
		})
		return 0, false
	}
	if err != nil {
		return unavailable(err)
	}

	source := "from:unbound"
	if request.From != nil {
		// A reason code cannot hold '@': `@environment` is recorded as
		// `environment`, which no stored key can collide with in this field.
		source = "from_provider:" + strings.TrimPrefix(request.From.Key, "@")
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/admin")
	if err != nil {
		return unavailable(err)
	}
	// The issuers are configuration, not personal data, but they can be long
	// URLs; the provider keys and the count say what moved.
	if _, err := appender.AppendRecord(ctx, tx, securityaudit.SecurityEvent{
		Type:   securityaudit.TypeIdentityLinkChanged,
		Action: ActionIdentityBindingRebound,
		Effect: securityaudit.EffectAllow,
		Reasons: []string{
			"provider_namespace:" + strings.TrimSuffix(request.Prefix, ":"),
			source,
			"to_provider:" + request.Target.Key,
			"rebound:" + strconv.FormatInt(rebound, 10),
		},
		PrincipalRef: "user:" + strconv.FormatInt(operator, 10),
		ResourceType: "identity_provider",
		ResourceID:   request.Target.Key,
	}); err != nil {
		return unavailable(err)
	}
	// A rebind under a sealed elevation is announced in this transaction
	// (ADR-0059 R2.8b).
	if err := securityaudit.RunTransactionHooks(ctx, tx); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return rebound, true
}
