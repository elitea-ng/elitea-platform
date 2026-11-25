package api

// What an elevation cannot do, enforced (ADR-0059 decision 5; story R2.8a,
// issue 2522). The rule is adminaccess.DecideElevation; this file finds the
// entry a request reaches, the way the token rule does
// (token_critical_actions.go), and applies it before the route's own gate.
//
// It acts on a credential that can hold an elevation only: a browser session
// with a server-side row (auth.Credential.ElevationSessionRef). For any other
// credential it is a pass-through with no lookup. For such a session on an
// administration action it reads the administration-mode resolution once
// (the route gate reads it too) and, when an elevation took part, decides
// from the entry's elevation value:
//
//   - one entry, or entries whose decisions agree: that decision (a refusal
//     is 403 elevation_restricted, a flag marks the admin audit row);
//   - classifier outcomes that disagree: the handler decides after it has
//     classified when the classifier is one of
//     adminaccess.ElevationRuleInHandler (the caller is put on the context
//     for it); otherwise the request is refused: the closed failure;
//   - a resolution that fails: refused when an entry could be refused, and
//     otherwise left to the route gate, which resolves again.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type elevationRestrictions struct {
	routes   chi.Routes
	resolver auth.PermissionResolver
}

// mark records the finished router; nothing writes it afterwards.
func (g *elevationRestrictions) mark(r chi.Routes) { g.routes = r }

func (g *elevationRestrictions) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.UserFromContext(r.Context())
		if !ok || g.routes == nil || principal.Credential().ElevationSessionRef() <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		candidates, rctx := actionCandidates(g.routes, r)
		if len(candidates) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		candidates, r = selectEntries(candidates, r, rctx)
		// Every entry allows an elevation: nothing to refuse or flag, so the
		// resolution is not read twice (the route gate reads it next).
		if !anyRestricted(candidates) {
			next.ServeHTTP(w, r)
			return
		}
		caller, resolved := g.elevationCaller(r, principal)
		if !resolved {
			if anyMayRestrict(candidates) {
				writeElevationRefusal(w, r, candidates, &adminaccess.Refusal{
					Code:    adminaccess.CodeElevationRestricted,
					Message: "the permissions of this session could not be read, so the action was refused",
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !caller.Elevated {
			next.ServeHTTP(w, r)
			return
		}
		decisions := make([]adminaccess.ElevationDecision, len(candidates))
		agree := true
		for i, entry := range candidates {
			decisions[i] = adminaccess.DecideElevation(entry, caller)
			if !decisions[i].Same(decisions[0]) {
				agree = false
			}
		}
		switch {
		case agree && decisions[0].Refusal != nil:
			writeElevationRefusal(w, r, candidates, decisions[0].Refusal)
		case agree:
			if decisions[0].Flag {
				adminaccess.FlagElevation(r.Context())
			}
			next.ServeHTTP(w, r)
		case adminaccess.ElevationRuleInHandler[candidates[0].Classifier]:
			next.ServeHTTP(w, r.WithContext(adminaccess.ContextWithElevationCaller(r.Context(), caller)))
		default:
			writeElevationRefusal(w, r, candidates, &adminaccess.Refusal{
				Code: adminaccess.CodeElevationRestricted,
				Message: "an elevation cannot call this administration route, whose restriction depends " +
					"on what the request changes",
			})
		}
	})
}

// elevationCaller reads the caller's resolution in the administration mode.
func (g *elevationRestrictions) elevationCaller(r *http.Request, principal auth.User) (adminaccess.ElevationCaller, bool) {
	if g.resolver == nil {
		return adminaccess.ElevationCaller{}, false
	}
	resolution, err := g.resolver.ResolvePermissions(r.Context(), principal, auth.PermissionModeAdministration, "")
	if err != nil {
		return adminaccess.ElevationCaller{}, false
	}
	return adminaccess.ElevationCallerOf(principal.Credential(), resolution), true
}

// anyRestricted reports whether an entry's elevation value is not allow.
func anyRestricted(entries []adminaccess.Action) bool {
	for _, entry := range entries {
		if entry.Elevation != adminaccess.ElevationAllow {
			return true
		}
	}
	return false
}

// anyMayRestrict reports whether an entry can refuse an elevation at all.
func anyMayRestrict(entries []adminaccess.Action) bool {
	for _, entry := range entries {
		if entry.Elevation == adminaccess.ElevationDeny || entry.Elevation == adminaccess.ElevationSealed {
			return true
		}
	}
	return false
}

// writeElevationRefusal names the entry, or the route when no single entry
// was picked.
func writeElevationRefusal(w http.ResponseWriter, r *http.Request, entries []adminaccess.Action, refusal *adminaccess.Refusal) {
	name := entries[0].Key()
	if len(entries) > 1 {
		name = entries[0].Method + " " + entries[0].Pattern
	}
	adminaccess.WriteElevationRefusal(w, r, name, refusal)
}
