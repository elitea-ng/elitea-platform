package adminaccess

// # What an elevation cannot do (ADR-0059 decision 5; story R2.8a, issue 2522)
//
// Every entry of the action table carries an elevation value. When the
// permission that admits a request came from an elevation (it is in the
// resolution's elevated part, not in the standing set), the value decides:
//
//   - allow:  the request goes on;
//   - flag:   it goes on, and its admin audit row is marked elevation_flagged
//     for the review;
//   - deny:   403 elevation_restricted, under any elevation;
//   - sealed: 403 elevation_restricted, except under a sealed elevation at
//     the super_admin level, where it goes on and is flagged: repairing
//     identity trust while nobody can sign in is what the sealed path is
//     for.
//
// The value is the entry's and never the permission's: one permission covers
// both sides of several lines of the table (a retention policy shortened or
// lengthened). An action the STANDING set already admits is not restricted
// here, which is the ADR text: the named rules that bind the elevated person
// whatever admitted the request are R2.8b's.
//
// The middleware of internal/api applies the rule to every entry it can pick
// from the request. On a route whose entries are split by a classifier only
// the handler can run, it applies the rule when every outcome gives the same
// answer, and otherwise defers to the handler when the classifier is one of
// ElevationRuleInHandler (the handler calls RestrictElevation after
// classifying), and refuses otherwise: the closed failure.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// CodeElevationRestricted is the stable code of the refusal.
const CodeElevationRestricted = "elevation_restricted"

// ElevationRestrictedAuditAction is the admin audit row's action of the
// refusal.
const ElevationRestrictedAuditAction = "admin.elevation_restricted"

// ElevationCaller is what the rule needs to know of an elevated caller.
type ElevationCaller struct {
	// Elevated: an active elevation took part in the resolution.
	Elevated bool
	// Standing is the caller's standing administration set.
	Standing []string
	// SealedSuperAdmin: the credential is a sealed session and an elevation
	// on the sealed path at the super_admin level took part.
	SealedSuperAdmin bool
}

// ElevationCallerOf answers the caller of a resolution of credential.
func ElevationCallerOf(credential auth.Credential, resolution auth.PermissionResolution) ElevationCaller {
	caller := ElevationCaller{Elevated: resolution.Elevated(), Standing: resolution.StandingPermissions()}
	if credential.IsSealed() {
		for _, elevation := range resolution.Elevations {
			if elevation.Path == ElevationPathSealed && elevation.Level == ElevationLevelSuperAdmin {
				caller.SealedSuperAdmin = true
			}
		}
	}
	return caller
}

// ElevationDecision is the rule's answer on one entry.
type ElevationDecision struct {
	// Refusal is set when the request is refused.
	Refusal *Refusal
	// Flag: the request goes on and its audit row is marked for the review.
	Flag bool
}

// Same reports whether two decisions give the same answer.
func (d ElevationDecision) Same(other ElevationDecision) bool {
	return (d.Refusal == nil) == (other.Refusal == nil) && d.Flag == other.Flag
}

// DecideElevation answers the rule on entry for caller.
func DecideElevation(entry Action, caller ElevationCaller) ElevationDecision {
	if !caller.Elevated || entry.Permission == "" || standingAdmits(caller.Standing, entry.Permission) {
		return ElevationDecision{}
	}
	switch entry.Elevation {
	case ElevationFlag:
		return ElevationDecision{Flag: true}
	case ElevationDeny:
		return ElevationDecision{Refusal: elevationRefusal(
			"an elevation cannot do this administration action: it needs the permission by a standing role")}
	case ElevationSealed:
		if caller.SealedSuperAdmin {
			return ElevationDecision{Flag: true}
		}
		return ElevationDecision{Refusal: elevationRefusal(
			"only a sealed break-glass elevation at the super_admin level can do this administration action")}
	}
	return ElevationDecision{}
}

// standingAdmits reports whether a standing set admits the permission of an
// entry: the name itself, or a legacy name it narrows, which the gates of
// ADR-0059 decision 2 accept beside it.
func standingAdmits(standing []string, permission string) bool {
	if slices.Contains(standing, permission) {
		return true
	}
	row, ok := Lookup(permission)
	if !ok {
		return false
	}
	for _, legacy := range row.Narrows {
		if slices.Contains(standing, legacy) {
			return true
		}
	}
	return false
}

func elevationRefusal(message string) *Refusal {
	return &Refusal{Code: CodeElevationRestricted, Message: message}
}

// ElevationRuleInHandler lists the classifiers whose handler applies the
// rule itself after classifying (RestrictElevation). On any other classified
// route whose outcomes disagree, the middleware refuses an elevated caller.
var ElevationRuleInHandler = map[Classifier]bool{
	ClassifyIdentityProvider: true,
	ClassifyRetention:        true,
	ClassifyGlobalProvider:   true,
}

type elevationCallerKey struct{}

// ContextWithElevationCaller records the elevated caller for a handler that
// applies the rule itself (ElevationRuleInHandler).
func ContextWithElevationCaller(ctx context.Context, caller ElevationCaller) context.Context {
	return context.WithValue(ctx, elevationCallerKey{}, caller)
}

// CheckElevationRule is the handler half of the rule: the decision for the
// caller the middleware recorded on ctx, on the entry the handler
// classified. The zero decision when no caller was recorded (the request is
// not elevated, or the middleware decided already).
func CheckElevationRule(ctx context.Context, method, pattern, action string) ElevationDecision {
	caller, ok := ctx.Value(elevationCallerKey{}).(ElevationCaller)
	if !ok {
		return ElevationDecision{}
	}
	entry, found := ActionFor(method, pattern, action)
	if !found {
		// The handler names an entry the table does not have: a defect, and
		// the closed failure.
		return ElevationDecision{Refusal: elevationRefusal("an elevation cannot do this administration action")}
	}
	return DecideElevation(entry, caller)
}

// RestrictElevation applies the handler half of the rule to the entry
// (method, pattern, action) the handler classified: a refusal is written
// (WriteElevationRefusal) and answers true, so the handler returns without
// writing; a flag marks the audit row.
func RestrictElevation(w http.ResponseWriter, r *http.Request, method, pattern, action string) bool {
	decision := CheckElevationRule(r.Context(), method, pattern, action)
	if decision.Refusal != nil {
		WriteElevationRefusal(w, r, Action{Method: method, Pattern: pattern, Name: action}.Key(), decision.Refusal)
		return true
	}
	if decision.Flag {
		FlagElevation(r.Context())
	}
	return false
}

// FlagElevation marks the admin audit row of the request for the review.
func FlagElevation(ctx context.Context) {
	audit.Annotate(ctx, audit.Annotation{ElevationFlagged: true})
}

// WriteElevationRefusal answers 403 with the stable code and annotates the
// admin audit row with the action name.
func WriteElevationRefusal(w http.ResponseWriter, r *http.Request, name string, refusal *Refusal) {
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     ElevationRestrictedAuditAction,
		EntityType: "admin_action",
		EntityName: name,
	})
	slog.WarnContext(r.Context(), "an elevation was refused an administration action",
		"code", refusal.Code, "action", name)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": refusal.Message, "code": refusal.Code})
}
