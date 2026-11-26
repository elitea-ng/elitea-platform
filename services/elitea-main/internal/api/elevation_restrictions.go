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
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type elevationRestrictions struct {
	routes   chi.Routes
	resolver auth.PermissionResolver
	// announcer writes the announcement of a critical change admitted under
	// an elevation (ADR-0059 R2.8b); nil refuses every such change.
	announcer *adminaccess.CriticalAnnouncer
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
		candidates, rctx := apimw.AdminActionCandidates(g.routes, r)
		if len(candidates) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		candidates, r = apimw.SelectAdminActions(candidates, r, rctx)
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
		// The announcement of a critical change (R2.8b) is armed on this
		// set, and the change's write runs it in its transaction.
		caller.Announcer = g.announcer
		r = r.WithContext(securityaudit.ContextWithTransactionHooks(r.Context()))
		decisions := make([]adminaccess.ElevationDecision, len(candidates))
		agree := true
		for i, entry := range candidates {
			decisions[i] = adminaccess.DecideElevation(entry, caller)
			if !decisions[i].Same(decisions[0]) {
				agree = false
			}
		}
		inHandler := adminaccess.ElevationRuleInHandler[candidates[0].Classifier]
		switch {
		case agree && decisions[0].Refusal != nil:
			writeElevationRefusal(w, r, candidates, decisions[0].Refusal)
		case agree && inHandler && mixedDanger(candidates):
			// The handler classifies, and arms the announcement when the
			// outcome is critical (adminaccess.RestrictElevation).
			if decisions[0].Flag {
				adminaccess.FlagElevation(r.Context())
			}
			g.serveAnnounced(w, r.WithContext(adminaccess.ContextWithElevationCaller(r.Context(), caller)), next)
		case agree:
			if refusal := armCritical(r, candidates, caller); refusal != nil {
				writeElevationRefusal(w, r, candidates, refusal)
				return
			}
			if decisions[0].Flag {
				adminaccess.FlagElevation(r.Context())
			}
			g.serveAnnounced(w, r, next)
		case inHandler:
			g.serveAnnounced(w, r.WithContext(adminaccess.ContextWithElevationCaller(r.Context(), caller)), next)
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

// anyRestricted reports whether an entry's elevation value is not allow, or
// the entry is critical (an allowed critical change is announced, R2.8b).
func anyRestricted(entries []adminaccess.Action) bool {
	for _, entry := range entries {
		if entry.Elevation != adminaccess.ElevationAllow || entry.Danger == adminaccess.DangerCritical {
			return true
		}
	}
	return false
}

// anyMayRestrict reports whether an entry can refuse an elevation at all: a
// deny or sealed value, or a critical entry, which is refused when it cannot
// be announced.
func anyMayRestrict(entries []adminaccess.Action) bool {
	for _, entry := range entries {
		if entry.Elevation == adminaccess.ElevationDeny || entry.Elevation == adminaccess.ElevationSealed ||
			entry.Danger == adminaccess.DangerCritical {
			return true
		}
	}
	return false
}

// mixedDanger reports entries of which some are critical and some are not:
// only the handler's classification tells which one the request is.
func mixedDanger(entries []adminaccess.Action) bool {
	critical := 0
	for _, entry := range entries {
		if entry.Danger == adminaccess.DangerCritical {
			critical++
		}
	}
	return critical > 0 && critical < len(entries)
}

// armCritical arms the announcement of an admitted critical change (R2.8b).
// Entries that are all critical arm once, for the first. Entries of mixed
// danger that no handler classifies are refused: which entry the request is
// cannot be told, and a critical change must not go unannounced (a governance
// row write, whose guardrail outcome is critical, is one today).
func armCritical(r *http.Request, entries []adminaccess.Action, caller adminaccess.ElevationCaller) *adminaccess.Refusal {
	if mixedDanger(entries) {
		return &adminaccess.Refusal{
			Code: adminaccess.CodeElevationRestricted,
			Message: "an elevation cannot call this administration route, whose danger depends on what the " +
				"request changes",
		}
	}
	for _, entry := range entries {
		if entry.Danger == adminaccess.DangerCritical {
			return adminaccess.ArmCriticalChange(r.Context(), entry, caller)
		}
	}
	return nil
}

// serveAnnounced serves the request and logs a critical change that was
// armed and never run by a transaction, on an answer that reports success:
// either a writer missing from the hook path or a write that changed nothing
// (an assignment the person already held), so it is a warning to look at,
// not a missed announcement by itself.
func (g *elevationRestrictions) serveAnnounced(w http.ResponseWriter, r *http.Request, next http.Handler) {
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(recorder, r)
	if recorder.status < http.StatusBadRequest && securityaudit.TransactionHooksPending(r.Context()) {
		slog.WarnContext(r.Context(), "a critical action under an elevation answered success without running its announcement",
			"method", r.Method, "path", r.URL.Path, "status", recorder.status)
	}
}

// statusRecorder keeps the status of the answer.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// Flush passes a flush through, for a handler that asserts http.Flusher on
// its writer directly.
func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the writer below.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// writeElevationRefusal names the entry, or the route when no single entry
// was picked.
func writeElevationRefusal(w http.ResponseWriter, r *http.Request, entries []adminaccess.Action, refusal *adminaccess.Refusal) {
	name := entries[0].Key()
	if len(entries) > 1 {
		name = entries[0].Method + " " + entries[0].Pattern
	}
	adminaccess.WriteElevationRefusal(w, r, name, refusal)
}
