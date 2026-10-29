package agentrelease

import (
	"reflect"
	"sort"
)

// Change is one entry of a JSON diff. Path is a JSON Pointer (RFC 6901)
// into the compared documents. Op is "add" (only After), "remove" (only
// Before) or "replace" (both). The absent side is JSON null.
type Change struct {
	Path   string `json:"path"`
	Op     string `json:"op"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// maxDiffDepth bounds the object recursion; deeper values compare whole.
const maxDiffDepth = 8

// Diff compares two JSON objects. Objects are compared key by key, in key
// order; any other value (a string, a number, an array) compares whole, so
// an edited array is one "replace". Equal documents give an empty list,
// never nil.
func Diff(before, after map[string]any) []Change {
	changes := []Change{}
	diffObjects(&changes, "", before, after, 0)
	return changes
}

func diffObjects(changes *[]Change, path string, before, after map[string]any, depth int) {
	keys := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for key := range before {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for key := range after {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		childPath := path + "/" + escapePointer(key)
		left, inBefore := before[key]
		right, inAfter := after[key]
		switch {
		case !inBefore:
			*changes = append(*changes, Change{Path: childPath, Op: "add", After: right})
		case !inAfter:
			*changes = append(*changes, Change{Path: childPath, Op: "remove", Before: left})
		default:
			leftObject, leftIsObject := left.(map[string]any)
			rightObject, rightIsObject := right.(map[string]any)
			if leftIsObject && rightIsObject && depth < maxDiffDepth {
				diffObjects(changes, childPath, leftObject, rightObject, depth+1)
				continue
			}
			if !equalJSON(left, right) {
				*changes = append(*changes, Change{Path: childPath, Op: "replace", Before: left, After: right})
			}
		}
	}
}

// equalJSON compares two decoded JSON values. Numbers decoded with
// UseNumber compare by their text; two encodings of one number (1 and 1.0)
// therefore differ, which only over-reports a change.
func equalJSON(left, right any) bool {
	return reflect.DeepEqual(left, right)
}
