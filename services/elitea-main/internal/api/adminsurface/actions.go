package adminsurface

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// IsWrite reports whether a surface entry can change something: every
// method but GET and HEAD. A route is classed by its effect, not by the class
// of the permission that gates it, so a POST behind a `read` permission, or one
// whose write permission is checked only in its handler, still needs an
// action entry; an effect-free POST (a validation, a check) has one with no
// danger.
func (e Entry) IsWrite() bool {
	return e.Method != http.MethodGet && e.Method != http.MethodHead
}

// NamesNoPermission reports whether every check of the entry reads the whole
// answer (KindWholeSet): the route asks whether the caller holds any
// administration permission, and requires none. The token mint of a native
// session is one: it refuses the holder of any.
func (e Entry) NamesNoPermission() bool {
	for _, check := range e.Checks {
		if check.Kind != KindWholeSet {
			return false
		}
	}
	return len(e.Checks) > 0
}

// SelectorValue answers the value a variant selects: the part of its name
// after the first space ("action delete", "section email"), or "" for the
// default variant.
func (e Entry) SelectorValue() string {
	_, value, _ := strings.Cut(e.Variant, " ")
	return value
}

// driveKey keys a route in the form the action table uses
// (adminaccess.RoutePattern), so a walked pattern and an entry meet.
func driveKey(method, pattern, selector string) string {
	return method + " " + adminaccess.RoutePattern(pattern) + "\x00" + selector
}

// ActionFailures answers one message per entry of the surface that can change
// something (IsWrite) with no action entry for its (method, pattern, selector
// value), unless it NamesNoPermission: an action entry names the permission
// the action requires, and such a route requires none (a test pins which
// writes those are); per action entry (a write's, or a restricted read's)
// whose permission none of the route's checks accepts, unless
// permissionExceptions holds the entry's Key with a reason; per stale
// exception; and per action entry whose route and selector value the surface
// does not have, so that the table and the surface stay one list.
func (res Result) ActionFailures(table adminaccess.Actions, permissionExceptions map[string]string) []string {
	var failures []string
	driven := map[string]bool{}
	usedException := map[string]bool{}
	for _, entry := range res.Surface {
		driven[driveKey(entry.Method, entry.Pattern, entry.SelectorValue())] = true
		if entry.NamesNoPermission() {
			continue
		}
		var matched []adminaccess.Action
		for _, action := range table.Route(entry.Method, entry.Pattern) {
			if action.Selector.Value == entry.SelectorValue() {
				matched = append(matched, action)
			}
		}
		// A read needs no entry; one the ADR restricts has one, and its
		// permission is held to the route's checks like a write's.
		if !entry.IsWrite() && len(matched) == 0 {
			continue
		}
		if len(matched) == 0 {
			failures = append(failures, fmt.Sprintf(
				"%s (variant %q) can change something on the administration surface and has no action entry: "+
					"add one to internal/adminaccess/action_rows.go", entry.Route, entry.Variant))
			continue
		}
		var accepted []string
		for _, check := range entry.Checks {
			accepted = append(accepted, check.Permissions...)
		}
		for _, action := range matched {
			switch {
			case slices.Contains(accepted, action.Permission):
			case permissionExceptions[action.Key()] != "":
				usedException[action.Key()] = true
			default:
				failures = append(failures, fmt.Sprintf(
					"%s requires %q, but the checks of the route accept only %v",
					action.Key(), action.Permission, accepted))
			}
		}
	}
	for key := range permissionExceptions {
		if !usedException[key] {
			failures = append(failures, fmt.Sprintf(
				"%s is declared as requiring a permission its route's checks do not show, but they show it "+
					"or the entry is gone; remove the exception", key))
		}
	}
	for _, action := range table.Entries() {
		if !driven[driveKey(action.Method, action.Pattern, action.Selector.Value)] {
			failures = append(failures, fmt.Sprintf(
				"%s has an action entry, but the administration surface has no such route or variant: "+
					"remove the entry, or reach the route", action.Key()))
		}
	}
	slices.Sort(failures)
	return slices.Compact(failures)
}
