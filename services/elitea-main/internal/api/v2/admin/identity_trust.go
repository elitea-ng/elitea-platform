package admin

// The super_admin gate on identity trust (issue 2558, ADR-0059 R3B-1).
//
// # Why these writes are global-administrator writes
//
// Two admin surfaces decide who a person IS, not what they may do:
//
//   - an identity provider's trust fields (identityproviders.TrustChanges):
//     whoever repoints the issuer, the metadata or the signing certificates at
//     an identity provider they control can assert the subject of any account,
//     a super_admin's included, and sign in as it;
//   - a SCIM client: its holder pushes any user, itself included, into any
//     bound group, and so into the role that group grants.
//
// Both are therefore equal to `super_admin`. Their route gates
// (`runtime.plugins`, `admin.auth.users`, and since ADR-0059 R0.4c
// `identity.sso.settings.manage`) are held by narrower roles too, so the
// handler checks the caller's own administration set, read from the database
// for each request (adminaccess.ResolveCaller), for
// adminaccess.SuperAdminPermission or adminaccess.IdentitySSOTrustManage. It
// fails closed: a caller that cannot be read is refused with
// adminaccess.CodeCallerUnresolved.
//
// # The split (ADR-0059 R0.4c)
//
// Trust actions: create or delete an identity provider, turn one on or off,
// change a trust field (identityproviders.FieldClasses) or replace its secret,
// re-bind accounts, and create, rotate or delete a SCIM client. Everything
// else (a rename, the scopes, the login domains, the name attribute, a SCIM
// client revoke, the group bindings) is a settings action, open to the route
// gate.
//
// # The refusal, and the audit rows
//
// The conventions are the role policy's (internal/adminaccess) (#2555): a 403 whose JSON carries `error`
// and a stable `code`, and an annotation on the audit row the request
// middleware writes for every admin write and every refusal (event type
// `api`), so the row names the domain action rather than only the route. A
// change that passes the gate is annotated too, with its own action.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The refusal codes. Clients key their message on these, so they are stable.
const (
	// CodeIdentityTrustSuperAdminRequired: the save creates an identity
	// provider or changes one of its trust fields, and the caller is not a
	// super_admin.
	CodeIdentityTrustSuperAdminRequired = "identity_provider_trust_super_admin_required"
	// CodeIdentityRebindSuperAdminRequired: the caller is not a super_admin
	// and asked to re-bind accounts to an identity provider.
	CodeIdentityRebindSuperAdminRequired = "identity_binding_super_admin_required"
	// CodeSCIMClientSuperAdminRequired: the caller is not a super_admin and
	// asked to create, rotate or delete a SCIM client.
	CodeSCIMClientSuperAdminRequired = "scim_client_super_admin_required"
)

// The audit actions of these writes.
const (
	AuditIdentityTrustRefused  = "identity_provider.trust_change_refused"
	AuditIdentityTrustChanged  = "identity_provider.trust_changed"
	AuditIdentityRebindRefused = "identity_binding.rebind_refused"
	AuditIdentityRebound       = "identity_binding.rebound"
	AuditSCIMClientRefused     = "scim_client.change_refused"
	AuditSCIMClientCreated     = "scim_client.created"
	AuditSCIMClientRotated     = "scim_client.rotated"
	AuditSCIMClientDeleted     = "scim_client.deleted"
)

// trustGate describes one gated write.
type trustGate struct {
	code          string
	message       string
	refusedAction string
	entityType    string
	entityName    string
	// fields are the trust fields the write changes, for the refusal body and
	// the log line. Empty for a write that is gated as a whole.
	fields []string
}

// requireIdentityTrust answers 403 and returns false unless the caller holds
// adminaccess.SuperAdminPermission or adminaccess.IdentitySSOTrustManage, the
// name of the identity trust half (ADR-0059 R0.4c). The registry seeds the
// trust name on super_admin alone and refuses it on every other role, so the
// two answer the same today; the name lets the action table and the audit
// state what the action needs.
//
// The route gates in front of these writes accept the settings name and the
// legacy names too, on purpose: a refusal of a trust action must be written
// here, with its code and its audit action, not as the route gate's bare 403.
func (h *Handler) requireIdentityTrust(w http.ResponseWriter, r *http.Request, gate trustGate) bool {
	caller, refusal := adminaccess.ResolveCaller(r, h.resolver)
	if refusal != nil {
		refusal.Message = "your own permissions could not be read, so the change was refused"
		writeTrustRefusal(w, r, gate, refusal)
		return false
	}
	if caller.SuperAdmin || caller.Administration[adminaccess.IdentitySSOTrustManage] {
		return true
	}
	// The caller is the STANDING set (ADR-0059 R2.1b), so no approved or
	// self elevation passes here: an approved online elevation never changes
	// identity trust (ADR-0059 decision 5, the rejected option "Let an
	// approved elevation change identity trust"). The one elevation that
	// may, a super_admin-level elevation on the sealed path, is read from the
	// full resolution of a sealed session only (story R2.8a), and the row is
	// flagged for the review.
	if h.sealedSuperAdminTrust(r) {
		adminaccess.FlagElevation(r.Context())
		return true
	}
	writeTrustRefusal(w, r, gate, &adminaccess.Refusal{Code: gate.code, Message: gate.message})
	return false
}

// sealedSuperAdminTrust reports whether the caller is a sealed session whose
// sealed super_admin elevation gives identity trust: the full
// administration-mode resolution, for the sealed credential kind only. Any
// other kind, a failed resolution, or a sealed session at the admin level
// answers false.
func (h *Handler) sealedSuperAdminTrust(r *http.Request) bool {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok || h.resolver == nil || !principal.Credential().IsSealed() {
		return false
	}
	resolution, err := h.resolver.ResolvePermissions(r.Context(), principal, auth.PermissionModeAdministration, "")
	if err != nil || resolution.UserID <= 0 {
		return false
	}
	if !adminaccess.ElevationCallerOf(principal.Credential(), resolution).SealedSuperAdmin {
		return false
	}
	return slices.Contains(resolution.ElevatedPermissions, adminaccess.SuperAdminPermission) ||
		slices.Contains(resolution.ElevatedPermissions, adminaccess.IdentitySSOTrustManage)
}

// writeTrustRefusal is adminaccess.Write with this surface's audit action and
// entity.
func writeTrustRefusal(w http.ResponseWriter, r *http.Request, gate trustGate, refusal *adminaccess.Refusal) {
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     gate.refusedAction,
		EntityType: gate.entityType,
		EntityName: gate.entityName,
	})
	slog.WarnContext(r.Context(), "identity trust change refused: super_admin required",
		"code", refusal.Code, "entity_type", gate.entityType, "entity", gate.entityName,
		"fields", gate.fields, "method", r.Method, "path", r.URL.Path)

	body := refusal.Body()
	if len(gate.fields) > 0 {
		body["fields"] = gate.fields
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(body)
}

// annotateTrustChange names an allowed change on the request's audit row, and
// logs it at warning level: a trust change is rare and always worth reading.
func annotateTrustChange(r *http.Request, action, entityType, entityName string, entityID *int64, fields []string) {
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		EntityName: entityName,
	})
	slog.WarnContext(r.Context(), "identity trust changed by a super_admin",
		"action", action, "entity_type", entityType, "entity", entityName, "fields", fields)
}
