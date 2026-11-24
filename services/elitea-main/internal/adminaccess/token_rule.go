package adminaccess

// # The token rule of the action table (ADR-0059 decision 7, story R1.2b)
//
// A personal access token with administration access (`scoped` or
// `inherit`, shared 0265) is refused on an action whose entry says
// `token: never`, which is the default of every critical action. A critical
// action that says `token: allowed` (the identity trust repair path of
// decision 5, until the sunset) is open only to an `inherit` token whose
// owner is a STANDING global administrator. The level and the rule come from
// the action table only, never from the permission.
//
// A token with `none` resolves no administration permission, so the route
// gate refuses it; the rule leaves it to the gate. Every other credential
// (a browser session, a native session, a run token, the system token) is
// not a personal access token and the rule does not apply: the
// administration set of R1.2a already decides what it holds.
//
// The middleware of internal/api applies the rule to every entry it can pick
// from the request alone. On a route whose entries are split by a classifier
// that only the handler can run, it applies the rule when every outcome gives
// the same answer, and otherwise refuses, unless the classifier is one whose
// handler applies the rule itself (TokenRuleInHandler), after classifying.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// CodeTokenCriticalAction is the stable code of the refusal.
const CodeTokenCriticalAction = "token_critical_action_refused"

// TokenRefusedAuditAction is the admin audit row's action of the refusal.
// The security signal of a refusal is access.authz_denied (ADR-0058 E2.6),
// not a new type.
const TokenRefusedAuditAction = "admin.token_refused"

// The admin_access values of a personal access token (shared 0265), as the
// administration-mode resolution reports them.
const (
	TokenAccessNone    = "none"
	TokenAccessScoped  = "scoped"
	TokenAccessInherit = "inherit"
)

// TokenCaller is what the rule needs to know of a personal access token.
type TokenCaller struct {
	// Access is the token row's admin_access.
	Access string
	// StandingGlobalAdmin: the owner is a global administrator by the
	// owner's standing roles, not by an elevation.
	StandingGlobalAdmin bool
}

// TokenRefusal answers the refusal of caller on entry, nil when the token may
// call it.
func TokenRefusal(entry Action, caller TokenCaller) *Refusal {
	switch caller.Access {
	case TokenAccessScoped, TokenAccessInherit:
	default:
		// none (or no row): the token holds no administration permission and
		// the route gate refuses it.
		return nil
	}
	refused := &Refusal{
		Code: CodeTokenCriticalAction,
		Message: "a personal access token cannot call this administration action: " +
			"sign in to the console to do it",
	}
	if entry.Token == TokenNever {
		return refused
	}
	if entry.Danger == DangerCritical {
		// token: allowed on a critical action is the written exception: an
		// inherit token of a standing global administrator only.
		if caller.Access == TokenAccessInherit && caller.StandingGlobalAdmin {
			return nil
		}
		refused.Message = "only a personal access token with inherit access, of a global administrator, " +
			"can call this administration action: sign in to the console to do it"
		return refused
	}
	return nil
}

// StandingGlobalAdmin answers whether a resolved administration set makes its
// owner a global administrator. Until R2.1 adds the standing-set call, the
// owner's resolved set IS the standing set (no elevation exists); R2.1
// replaces this variable's value with the standing-set call, and the rule
// does not change.
var StandingGlobalAdmin = func(_ context.Context, ownerSet []string) bool {
	return slices.Contains(ownerSet, SuperAdminPermission)
}

// TokenRuleInHandler lists the classifiers whose handler applies the token
// rule itself after classifying (CheckTokenRule). On any other classified
// route whose outcomes disagree, the middleware refuses a token: the closed
// failure.
var TokenRuleInHandler = map[Classifier]bool{
	ClassifyIdentityProvider: true,
	// The gateway governance rows, the global provider update and the
	// retention writes (issue 2993). Each handler classifies from the stored
	// row and the body, and makes its write conditional on what it
	// classified, so a concurrent change cannot turn the harmless outcome
	// into the critical one between the check and the write.
	ClassifyGovernanceRow:  true,
	ClassifyGlobalProvider: true,
	ClassifyRetention:      true,
}

type tokenCallerKey struct{}

// ContextWithTokenCaller records the token caller for a handler that applies
// the rule itself (TokenRuleInHandler).
func ContextWithTokenCaller(ctx context.Context, caller TokenCaller) context.Context {
	return context.WithValue(ctx, tokenCallerKey{}, caller)
}

// CheckTokenRule is the handler half of the rule: the refusal of the token
// caller the middleware recorded on ctx, on the entry the handler classified.
// nil when the request is not a token's, or the token may call the entry.
func CheckTokenRule(ctx context.Context, method, pattern, action string) *Refusal {
	caller, ok := ctx.Value(tokenCallerKey{}).(TokenCaller)
	if !ok {
		return nil
	}
	entry, found := ActionFor(method, pattern, action)
	if !found {
		// The handler names an entry the table does not have: a defect, and
		// the closed failure.
		return &Refusal{Code: CodeTokenCriticalAction,
			Message: "a personal access token cannot call this administration action"}
	}
	return TokenRefusal(entry, caller)
}

// TokenRefused answers whether the token caller of ctx is refused the entry
// (method, pattern, action): CheckTokenRule as a question, for a handler that
// narrows its write by the answer before it can classify (a stored row read
// in the write's own statement).
func TokenRefused(ctx context.Context, method, pattern, action string) bool {
	return CheckTokenRule(ctx, method, pattern, action) != nil
}

// RefuseToken applies the handler half of the rule to the entry (method,
// pattern, action) the handler classified. When the token caller of r is
// refused it, RefuseToken writes the 403 the middleware writes, annotates
// the admin audit row with TokenRefusedAuditAction and answers true: the
// handler returns without writing.
func RefuseToken(w http.ResponseWriter, r *http.Request, method, pattern, action string) bool {
	refusal := CheckTokenRule(r.Context(), method, pattern, action)
	if refusal == nil {
		return false
	}
	WriteTokenRefusal(w, r, method, pattern, action, refusal)
	return true
}

// WriteTokenRefusal writes the token rule's refusal of the entry (method,
// pattern, action) the way the middleware of internal/api writes it.
func WriteTokenRefusal(w http.ResponseWriter, r *http.Request, method, pattern, action string, refusal *Refusal) {
	name := Action{Method: method, Pattern: pattern, Name: action}.Key()
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     TokenRefusedAuditAction,
		EntityType: "admin_action",
		EntityName: name,
	})
	slog.WarnContext(r.Context(), "a personal access token was refused on an administration action",
		"code", refusal.Code, "action", name)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": refusal.Message, "code": refusal.Code})
}

// CriticalEntries lists the critical entries of the action table, sorted, for
// the upgrade note and its guard.
func CriticalEntries() []Action {
	var out []Action
	for _, entry := range ActionTable().Entries() {
		if entry.Danger == DangerCritical {
			out = append(out, entry)
		}
	}
	return out
}
