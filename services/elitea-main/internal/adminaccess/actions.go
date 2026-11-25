package adminaccess

// # The action table
//
// Danger, whether a reason is required, what an elevation may do and whether
// a token may call are properties of an ACTION (ADR-0059 decision 1, story
// R0.1c). One permission covers actions of different danger: retention.manage
// lengthens and shortens a retention policy, admin.auth.users deletes a user
// and assigns an administrator role. The action table holds one entry per
// (method, route pattern, action) that changes something on the
// administration surface.
//
// This file only declares. Nothing enforces the table yet: the token refusal
// is R1.2, the elevation restriction R2.8, the reason R3.8 and recent
// authentication for a critical action R4.1. Each reads the entry through
// ActionFor.
//
// # Shape of an entry
//
//   - A route with one action has one entry whose Name is "".
//   - A route that multiplexes actions on a part of the request (an `action`
//     field of the body, a `{plugin}` path segment) has one entry per value,
//     and the entry's Selector says which part and which value. The
//     administration surface test drives the route once per value.
//   - An action whose danger depends on what the request changes (shortening
//     versus lengthening a retention policy) is split into one entry per
//     outcome of a Classifier. The classifier functions are in
//     action_classifiers.go; ActionName joins a selector value and an outcome
//     into the entry's Name.
//
// A GET or HEAD changes nothing, so it is classed `read` whatever permission
// gates it and needs no entry. A read has one only when the ADR restricts it
// beyond its gate: reading the sealed credential list is `deny` under an
// elevation and `never` for a token (ADR-0059 decision 5, "What an elevation
// cannot do"; story R2.17), and the access posture states that a scoped token
// may read it (R2.14a). Such an entry has no danger, no reason, no selector
// and no classifier, and the token and elevation rules apply it like any
// other (read). A POST that changes nothing (a validation, a connection
// check) still has one, with no danger.
//
// An entry on a pattern with a `{mode}` segment describes the request in the
// `administration` mode only. The same route in the default mode is not on
// the administration surface (the user-facing project delete), and a gate
// that reads the table checks the mode before it looks the entry up.

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// Danger is the danger level of an action (ADR-0059 decision 6, "Dangerous
// actions").
type Danger string

// The danger levels. DangerNone is the zero value.
const (
	DangerNone      Danger = ""
	DangerDangerous Danger = "dangerous"
	DangerCritical  Danger = "critical"
)

// Elevation is what an elevation may do with an action (ADR-0059 decision 5,
// "What an elevation cannot do").
type Elevation string

// The elevation values.
const (
	// ElevationAllow: an elevation that holds the permission may do it.
	ElevationAllow Elevation = "allow"
	// ElevationFlag: allowed, and marked for the review.
	ElevationFlag Elevation = "flag"
	// ElevationDeny: 403 elevation_restricted under any elevation.
	ElevationDeny Elevation = "deny"
	// ElevationSealed: only a sealed elevation at the super_admin level.
	ElevationSealed Elevation = "sealed"
)

// TokenRule says whether a personal access token may call an action
// (ADR-0059 decision 7, "Tokens and critical actions").
type TokenRule string

// The token rules.
const (
	TokenAllowed TokenRule = "allowed"
	TokenNever   TokenRule = "never"
)

// Selector says which part of a request picks an action on a route that
// multiplexes several.
type Selector struct {
	// In is "body" (a top-level JSON field) or "path" (a placeholder).
	In string
	// Field is the JSON field or the placeholder name.
	Field string
	// Value is the value that selects the action.
	Value string
}

// The two places a selector reads.
const (
	SelectBody = "body"
	SelectPath = "path"
)

// Body answers the smallest JSON body that selects the action, or "" for a
// path selector.
func (s Selector) Body() string {
	if s.In != SelectBody {
		return ""
	}
	return fmt.Sprintf("{%q:%q}", s.Field, s.Value)
}

// Action is one entry of the action table.
type Action struct {
	Method  string
	Pattern string
	// Name names the action on a route that carries several; "" on a route
	// with one action. ActionName builds it.
	Name string
	// Selector is set on a route that multiplexes actions on a part of the
	// request.
	Selector Selector
	// Classifier names the request-dependent classifier whose outcome is the
	// last part of Name; "" when the request does not change the danger.
	Classifier Classifier
	// Permission is the administration permission the action requires. Where
	// a route gate and an in-handler check both decide, it is the narrower.
	// Where a gate accepts a narrow name of ADR-0059 decision 2 beside the
	// legacy name it narrows, it is the narrow name.
	Permission string
	Danger     Danger
	// Reason says whether the action asks for a reason (R3.8). Every
	// dangerous and critical action does.
	Reason    bool
	Elevation Elevation
	Token     TokenRule
	// TokenReason is the written reason a critical action says
	// `token: allowed`. Required then, refused otherwise.
	TokenReason string
}

// Key answers "METHOD pattern" or "METHOD pattern action".
func (a Action) Key() string {
	if a.Name == "" {
		return a.Method + " " + a.Pattern
	}
	return a.Method + " " + a.Pattern + " " + a.Name
}

// ActionName joins a selector value and a classifier outcome into the Name of
// an entry. Either may be "".
func ActionName(selector, outcome string) string {
	switch {
	case selector == "":
		return outcome
	case outcome == "":
		return selector
	}
	return selector + ":" + outcome
}

// Actions is an immutable action table.
type Actions struct {
	entries []Action
	byKey   map[string]int
}

// NewActions validates entries against the permission table and returns them
// sorted by pattern, method and name. It fills the defaults: Reason for a
// dangerous or critical action, `allow` for an elevation a non-critical
// action leaves empty, `never` for the token rule of a critical action and
// `allowed` for any other.
func NewActions(entries []Action, permissions Table) (Actions, error) {
	sorted := make([]Action, len(entries))
	for i, entry := range entries {
		entry.Pattern = RoutePattern(entry.Pattern)
		filled, err := fill(entry, permissions)
		if err != nil {
			return Actions{}, err
		}
		sorted[i] = filled
	}
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Pattern != b.Pattern {
			return a.Pattern < b.Pattern
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Name < b.Name
	})
	byKey := make(map[string]int, len(sorted))
	routes := map[string][]Action{}
	for i, entry := range sorted {
		if _, duplicate := byKey[entry.Key()]; duplicate {
			return Actions{}, fmt.Errorf("adminaccess: action %q appears twice", entry.Key())
		}
		byKey[entry.Key()] = i
		route := entry.Method + " " + entry.Pattern
		routes[route] = append(routes[route], entry)
	}
	for route, group := range routes {
		if err := validateRoute(route, group); err != nil {
			return Actions{}, err
		}
	}
	return Actions{entries: sorted, byKey: byKey}, nil
}

func fill(a Action, permissions Table) (Action, error) {
	key := a.Key()
	switch a.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	case http.MethodGet, http.MethodHead:
		// A read changes nothing: no danger, so no reason, and one action.
		if a.Danger != DangerNone || a.Reason || a.Selector != (Selector{}) || a.Classifier != "" || a.Name != "" {
			return a, fmt.Errorf("adminaccess: action %q: a %s changes nothing; its entry has no danger, "+
				"reason, selector, classifier or name", key, a.Method)
		}
	default:
		return a, fmt.Errorf("adminaccess: action %q has the unknown method %q", key, a.Method)
	}
	if !strings.HasPrefix(a.Pattern, "/") {
		return a, fmt.Errorf("adminaccess: action %q has no route pattern", key)
	}
	if _, ok := permissions.Lookup(a.Permission); !ok {
		return a, fmt.Errorf("adminaccess: action %q requires %q, which is not in the permission table", key, a.Permission)
	}
	switch a.Danger {
	case DangerNone, DangerDangerous, DangerCritical:
	default:
		return a, fmt.Errorf("adminaccess: action %q has the unknown danger %q", key, a.Danger)
	}
	if a.Danger != DangerNone {
		a.Reason = true
	}
	switch a.Elevation {
	case ElevationAllow, ElevationFlag, ElevationDeny, ElevationSealed:
	case "":
		if a.Danger == DangerCritical {
			return a, fmt.Errorf("adminaccess: action %q is critical and declares no elevation value "+
				"(ADR-0059 decision 5, \"What an elevation cannot do\")", key)
		}
		a.Elevation = ElevationAllow
	default:
		return a, fmt.Errorf("adminaccess: action %q has the unknown elevation %q", key, a.Elevation)
	}
	switch a.Token {
	case TokenAllowed, TokenNever:
	case "":
		a.Token = TokenAllowed
		if a.Danger == DangerCritical {
			a.Token = TokenNever
		}
	default:
		return a, fmt.Errorf("adminaccess: action %q has the unknown token rule %q", key, a.Token)
	}
	switch {
	case a.Danger == DangerCritical && a.Token == TokenAllowed && strings.TrimSpace(a.TokenReason) == "":
		return a, fmt.Errorf("adminaccess: action %q is critical and says token: allowed without a written reason", key)
	case a.TokenReason != "" && (a.Danger != DangerCritical || a.Token != TokenAllowed):
		return a, fmt.Errorf("adminaccess: action %q has a token reason, which only a critical action "+
			"that says token: allowed carries", key)
	}
	switch a.Selector.In {
	case "":
		if a.Selector != (Selector{}) {
			return a, fmt.Errorf("adminaccess: action %q has a selector with no place to read", key)
		}
	case SelectBody, SelectPath:
		if a.Selector.Field == "" || a.Selector.Value == "" {
			return a, fmt.Errorf("adminaccess: action %q has a selector with no field or no value", key)
		}
		if a.Selector.In == SelectPath && !strings.Contains(a.Pattern, "{"+a.Selector.Field+"}") {
			return a, fmt.Errorf("adminaccess: action %q selects on the path placeholder %q, which its pattern lacks",
				key, a.Selector.Field)
		}
	default:
		return a, fmt.Errorf("adminaccess: action %q has a selector in %q, want %q or %q",
			key, a.Selector.In, SelectBody, SelectPath)
	}
	var outcome string
	if a.Classifier != "" {
		outcomes, ok := classifierOutcomes[a.Classifier]
		if !ok {
			return a, fmt.Errorf("adminaccess: action %q names the unknown classifier %q", key, a.Classifier)
		}
		outcome = strings.TrimPrefix(a.Name, a.Selector.Value+":")
		if a.Selector.Value == "" {
			outcome = a.Name
		}
		if !slices.Contains(outcomes, outcome) {
			return a, fmt.Errorf("adminaccess: action %q: %q is not an outcome of the classifier %q (%v)",
				key, outcome, a.Classifier, outcomes)
		}
	}
	if want := ActionName(a.Selector.Value, outcome); a.Name != want {
		return a, fmt.Errorf("adminaccess: action %q should be named %q (selector value, then classifier outcome)",
			key, want)
	}
	return a, nil
}

// validateRoute checks the entries of one (method, pattern): one entry with
// no name, or entries that each name their action; one selector field; and,
// for every classified selector value, one entry per outcome.
func validateRoute(route string, group []Action) error {
	if len(group) > 1 && slices.ContainsFunc(group, func(a Action) bool { return a.Name == "" }) {
		return fmt.Errorf("adminaccess: %s has several actions, and one of them has no name", route)
	}
	field := group[0].Selector.In + " " + group[0].Selector.Field
	classified := map[string]Classifier{}
	seen := map[string][]string{}
	for _, a := range group {
		if a.Selector.In+" "+a.Selector.Field != field {
			return fmt.Errorf("adminaccess: %s selects its actions on more than one part of the request", route)
		}
		if a.Classifier == "" {
			continue
		}
		if c, ok := classified[a.Selector.Value]; ok && c != a.Classifier {
			return fmt.Errorf("adminaccess: %s splits %q with two classifiers", route, a.Selector.Value)
		}
		classified[a.Selector.Value] = a.Classifier
		seen[a.Selector.Value] = append(seen[a.Selector.Value], strings.TrimPrefix(a.Name, a.Selector.Value+":"))
	}
	for value, c := range classified {
		for _, outcome := range classifierOutcomes[c] {
			if !slices.Contains(seen[value], outcome) {
				return fmt.Errorf("adminaccess: %s has no entry for the outcome %q of the classifier %q",
					route, ActionName(value, outcome), c)
			}
		}
		for _, a := range group {
			if a.Selector.Value == value && a.Classifier == "" {
				return fmt.Errorf("adminaccess: %s has an unclassified entry beside the classified %q", route, value)
			}
		}
	}
	return nil
}

// Entries returns a copy of the entries, sorted.
func (t Actions) Entries() []Action { return append([]Action(nil), t.entries...) }

// Route returns the entries of one (method, pattern), sorted by name. pattern
// may be in either form RoutePattern accepts.
func (t Actions) Route(method, pattern string) []Action {
	pattern = RoutePattern(pattern)
	var out []Action
	for _, a := range t.entries {
		if a.Method == method && a.Pattern == pattern {
			out = append(out, a)
		}
	}
	return out
}

// Lookup returns the entry of (method, pattern, action). action is "" on a
// route with one action. pattern may be in either form RoutePattern accepts.
func (t Actions) Lookup(method, pattern, action string) (Action, bool) {
	i, ok := t.byKey[Action{Method: method, Pattern: RoutePattern(pattern), Name: action}.Key()]
	if !ok {
		return Action{}, false
	}
	return t.entries[i], true
}

// actions is the action table of this service, built once like the
// permission table and for the same reason: a bad entry must not serve a gate.
var actions = mustActions(actionRows(), registry)

func mustActions(entries []Action, permissions Table) Actions {
	table, err := NewActions(entries, permissions)
	if err != nil {
		panic(fmt.Sprintf("invalid action table: fix the entry in "+
			"services/elitea-main/internal/adminaccess/action_rows.go: %v", err))
	}
	return table
}

// ActionTable returns the action table of this service.
func ActionTable() Actions { return actions }

// ActionFor returns the entry of (method, pattern, action) of this service.
// pattern is the chi route pattern, as chi.Walk lists it or as
// chi.RouteContext(ctx).RoutePattern() answers it during a request; action is
// "" on a route with one action, and otherwise ActionName of the selector
// value and the classifier outcome.
//
// ActionFor does not know the request's mode. A gate that has it calls
// ActionForRequest, which answers no entry outside the administration mode.
func ActionFor(method, pattern, action string) (Action, bool) {
	return actions.Lookup(method, pattern, action)
}

// ActionForRequest is ActionFor for a request whose `{mode}` the gate has
// read: administrationMode says whether it is the administration mode. On a
// pattern with a `{mode}` segment it answers no entry outside that mode: the
// same route in the default mode (the user-facing project delete) is not on
// the administration surface. The caller states the mode, so the lookup cannot
// match the default mode by accident. (A bool, not the mode's name: this
// package never names the mode, which keeps it out of the internal listener
// scan of internal/transport.)
func ActionForRequest(administrationMode bool, method, pattern, action string) (Action, bool) {
	if !administrationMode && strings.Contains(pattern, "{mode}") {
		return Action{}, false
	}
	return ActionFor(method, pattern, action)
}

// RoutePattern answers pattern in the form chi's RoutePattern() gives during
// a request: a sub-router mounted at "/" leaves "/*/" in the pattern chi.Walk
// lists, which chi collapses to "/", and a trailing "/" is dropped. Entries
// are keyed in this form, so a walked pattern and a request's pattern find
// the same entry.
func RoutePattern(pattern string) string {
	for strings.Contains(pattern, "/*/") {
		pattern = strings.ReplaceAll(pattern, "/*/", "/")
	}
	if pattern != "/" {
		pattern = strings.TrimSuffix(pattern, "//")
		pattern = strings.TrimSuffix(pattern, "/")
	}
	return pattern
}
