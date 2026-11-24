package api

// The token rule of the action table, enforced (ADR-0059 decision 7, "Tokens
// and critical actions", story R1.2b, issue 2592). The rule itself is
// adminaccess.TokenRefusal; this file finds the entry a request reaches and
// applies it, before the route's own gate.
//
// It acts on personal access tokens only (auth.CredentialKindToken). For any
// other credential it is a pass-through with no lookup. For a token, it finds
// the route pattern with the finished router's own lookup (as the
// maintenance gate does, maintenance_routes.go), picks the entry of the
// action table, and, when the entry may refuse the token, reads the token's
// admin_access once through the administration-mode resolution.
//
// Picking the entry:
//
//   - one entry with no name: that entry;
//   - a body selector (the `action` field of POST /admin/auth_users/{mode}):
//     the body is read and put back whole for the handler. The field is
//     matched without regard to case, as the handler's struct decode matches
//     it;
//   - a path selector: the placeholder's value;
//   - a selector value the request does not state unambiguously (no field,
//     two spellings with different values, a body that is not one JSON
//     object, a body over maxTokenRuleBody) or that the table does not list:
//     every entry of the route, so a route with a critical entry refuses the
//     token rather than trust the handler to read the value the same way;
//   - classifier outcomes the request alone cannot tell apart: when every
//     outcome gives the same answer, that answer; otherwise, when the
//     classifier's handler applies the rule itself
//     (adminaccess.TokenRuleInHandler), the token caller is put on the
//     context for it; otherwise the token is refused: the closed failure.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// maxTokenRuleBody bounds the body read to find a body selector's value. A
// larger body is put back whole and its value counts as unknown: the
// multiplexed routes take small JSON bodies.
const maxTokenRuleBody = 1 << 20

type tokenCriticalActions struct {
	routes   chi.Routes
	resolver auth.PermissionResolver
}

// mark records the finished router; nothing writes it afterwards.
func (g *tokenCriticalActions) mark(r chi.Routes) { g.routes = r }

func (g *tokenCriticalActions) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.UserFromContext(r.Context())
		if !ok || principal.CredentialKind != auth.CredentialKindToken || g.routes == nil {
			next.ServeHTTP(w, r)
			return
		}
		candidates, rctx := g.candidates(r)
		if len(candidates) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		candidates, r = selectEntries(candidates, r, rctx)
		if !anyMayRefuse(candidates) {
			next.ServeHTTP(w, r)
			return
		}
		caller, resolved := g.tokenCaller(r, principal)
		if !resolved {
			refuseToken(w, r, candidates, &adminaccess.Refusal{
				Code:    adminaccess.CodeTokenCriticalAction,
				Message: "the access of this personal access token could not be read, so the action was refused",
			})
			return
		}
		refusals := make([]*adminaccess.Refusal, len(candidates))
		agree := true
		for i, entry := range candidates {
			refusals[i] = adminaccess.TokenRefusal(entry, caller)
			if (refusals[i] == nil) != (refusals[0] == nil) {
				agree = false
			}
		}
		switch {
		case agree && refusals[0] == nil:
			next.ServeHTTP(w, r)
		case agree:
			refuseToken(w, r, candidates, refusals[0])
		case adminaccess.TokenRuleInHandler[candidates[0].Classifier]:
			next.ServeHTTP(w, r.WithContext(adminaccess.ContextWithTokenCaller(r.Context(), caller)))
		default:
			refuseToken(w, r, candidates, &adminaccess.Refusal{
				Code: adminaccess.CodeTokenCriticalAction,
				Message: "a personal access token cannot call this administration route, whose danger depends " +
					"on what the request changes: sign in to the console to do it",
			})
		}
	})
}

// candidates answers the entries of the (method, pattern) the request routes
// to, with the route context of the lookup (its path parameters).
func (g *tokenCriticalActions) candidates(r *http.Request) ([]adminaccess.Action, *chi.Context) {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	rctx := chi.NewRouteContext()
	pattern := g.routes.Find(rctx, r.Method, path)
	if pattern == "" {
		return nil, rctx
	}
	pattern = adminaccess.RoutePattern(pattern)
	if strings.Contains(pattern, "{mode}") && rctx.URLParam("mode") != auth.PermissionModeAdministration {
		// The same route in another mode is not on the administration
		// surface (adminaccess.ActionForRequest).
		return nil, rctx
	}
	return adminaccess.ActionTable().Route(r.Method, pattern), rctx
}

// selectEntries narrows the entries of a route that multiplexes actions to
// the request's action. A value it cannot read, or one the table does not
// list, keeps every entry of the route: the closed failure. The request it
// returns carries the body it read.
func selectEntries(entries []adminaccess.Action, r *http.Request, rctx *chi.Context) ([]adminaccess.Action, *http.Request) {
	selector := entries[0].Selector
	var value string
	switch selector.In {
	case "":
		return entries, r
	case adminaccess.SelectPath:
		value = rctx.URLParam(selector.Field)
	case adminaccess.SelectBody:
		value, r = bodyField(r, selector.Field)
	}
	var out []adminaccess.Action
	for _, entry := range entries {
		if value != "" && entry.Selector.Value == value {
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return entries, r
	}
	return out, r
}

// bodyField reads the top-level string field of a JSON body, matching its
// name without regard to case as encoding/json does for a struct, and puts
// the whole body back. A body over maxTokenRuleBody, a body that is not one
// JSON object, a field that is not a string, and two spellings of the field
// with different values answer "": unknown.
func bodyField(r *http.Request, field string) (string, *http.Request) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", r
	}
	original := r.Body
	raw, err := io.ReadAll(io.LimitReader(original, maxTokenRuleBody+1))
	restored := r.Clone(r.Context())
	restored.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), original), original}
	if err != nil || len(raw) > maxTokenRuleBody {
		return "", restored
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return "", restored
	}
	value, seen := "", false
	for key, encoded := range body {
		if !strings.EqualFold(key, field) {
			continue
		}
		var spelled string
		if json.Unmarshal(encoded, &spelled) != nil || (seen && spelled != value) {
			return "", restored
		}
		value, seen = spelled, true
	}
	return value, restored
}

// anyMayRefuse reports whether an entry can refuse a token at all: a critical
// one, or one that says never.
func anyMayRefuse(entries []adminaccess.Action) bool {
	for _, entry := range entries {
		if entry.Danger == adminaccess.DangerCritical || entry.Token == adminaccess.TokenNever {
			return true
		}
	}
	return false
}

// tokenCaller reads the token's admin_access and whether its owner is a
// standing global administrator, through the administration-mode resolution
// the route gate makes too.
func (g *tokenCriticalActions) tokenCaller(r *http.Request, principal auth.User) (adminaccess.TokenCaller, bool) {
	if g.resolver == nil {
		return adminaccess.TokenCaller{}, false
	}
	resolution, err := g.resolver.ResolvePermissions(r.Context(), principal,
		auth.PermissionModeAdministration, "")
	if err != nil {
		return adminaccess.TokenCaller{}, false
	}
	return adminaccess.TokenCaller{
		Access:              resolution.TokenAdminAccess,
		StandingGlobalAdmin: adminaccess.StandingGlobalAdmin(r.Context(), resolution.StandingPermissions()),
	}, true
}

// refuseToken answers 403 with the stable code and annotates the admin audit
// row of the request with the entry, or the route when no single entry was
// picked.
func refuseToken(w http.ResponseWriter, r *http.Request, entries []adminaccess.Action, refusal *adminaccess.Refusal) {
	name := entries[0].Key()
	if len(entries) > 1 {
		name = entries[0].Method + " " + entries[0].Pattern
	}
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     adminaccess.TokenRefusedAuditAction,
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
