package adminsurface

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// IsWrite reports whether a surface entry is classed `write` or `approve`:
// its method can change something (a GET or HEAD is `read` whatever gates it)
// and one of its checks accepts a permission that the permission table
// classes `write` or `approve`.
func (e Entry) IsWrite(permissions adminaccess.Table) bool {
	if e.Method == http.MethodGet || e.Method == http.MethodHead {
		return false
	}
	for _, check := range e.Checks {
		for _, name := range check.Permissions {
			if row, ok := permissions.Lookup(name); ok && row.Class != adminaccess.ClassRead {
				return true
			}
		}
	}
	return false
}

// SelectorValue answers the value a variant selects: the part of its name
// after the first space ("action delete", "section email"), or "" for the
// default variant.
func (e Entry) SelectorValue() string {
	_, value, _ := strings.Cut(e.Variant, " ")
	return value
}

func driveKey(method, pattern, selector string) string {
	return method + " " + pattern + "\x00" + selector
}

// ActionFailures answers one message per write entry of the surface (IsWrite)
// with no action entry for its (method, pattern, selector value); per action
// entry whose permission none of the route's checks accepts, unless
// permissionExceptions holds the entry's Key with a reason; per stale
// exception; and per action entry whose route and selector value the surface
// does not have, so that the table and the surface stay one list.
func (res Result) ActionFailures(table adminaccess.Actions, permissions adminaccess.Table,
	permissionExceptions map[string]string) []string {
	var failures []string
	driven := map[string]bool{}
	usedException := map[string]bool{}
	for _, entry := range res.Surface {
		driven[driveKey(entry.Method, entry.Pattern, entry.SelectorValue())] = true
		if !entry.IsWrite(permissions) {
			continue
		}
		var matched []adminaccess.Action
		for _, action := range table.Route(entry.Method, entry.Pattern) {
			if action.Selector.Value == entry.SelectorValue() {
				matched = append(matched, action)
			}
		}
		if len(matched) == 0 {
			failures = append(failures, fmt.Sprintf(
				"%s (variant %q) is a write route of the administration surface with no action entry: "+
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
