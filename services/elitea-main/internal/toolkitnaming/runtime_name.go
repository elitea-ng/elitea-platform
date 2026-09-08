// Package toolkitnaming holds the ONE rule that turns the name a person gave a
// toolkit into the identifier the runtime addresses that toolkit's tools by.
//
// # Why this package exists
//
// The rule was written five times, and one copy disagreed with the other four.
//
// The four that agree — index admission
// (internal/application/indexing), the tool-run path
// (internal/application/toolkitcalltool), the built-in name deriver
// (internal/runtimecomposition) and the Python worker's SDK adapter
// (services/elitea-worker-python/src/elitea_worker/agents/sdk_adapter.py) —
// keep `_`, `.` and `-` and then fold `.` into `_`. The web client keeps the
// same rule in three places
// (features/toolkits/lib/helpers/toolkits.helpers.ts,
// features/agents/lib/toolkitLabel.ts and the pipeline editor's
// useGetToolkitNameFromSchema.ts).
//
// The fifth copy lived in the toolkit DETAILS route and kept alphanumerics
// only. So for every toolkit whose name carried a `-`, `_` or `.` — which is
// most of them, because the create form encourages a readable name — the
// details route reported an identifier the runtime never uses. A user reading
// the details page to learn what to call the toolkit in an instruction was
// told `autotestgithubtoolkitv1` for a toolkit the agent addresses as
// `autotestgithubtoolkit_v1`, and the instruction then named a tool that does
// not exist.
//
// Nothing here decides POLICY. It states the existing runtime rule once so a
// sixth copy cannot drift, and every caller in this service now goes through
// RuntimeName.
package toolkitnaming

import (
	"regexp"
	"strings"
)

// runtimeNameSanitizer drops every character the runtime identifier may not
// carry. It keeps `_`, `.` and `-`; the dot is folded separately, below,
// because the SDK's own tool registry uses `_` where a name held a dot.
var runtimeNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// RuntimeName is the identifier the runtime addresses a toolkit's tools by.
//
// storedName is the `name` column of the p_{project}.elitea_tools row.
// toolkitType is the row's `type`, and is used when the row carries no name at
// all — which is what the runtime does, so a details route that reported an
// empty string there would disagree again for a different reason.
//
// Truncation is deliberately NOT applied here. Only the built-in name deriver
// has a per-type maximum length to apply (it comes from the toolkit schema
// snapshot, which this package must not depend on), and it applies that bound
// to this function's answer.
func RuntimeName(storedName, toolkitType string) string {
	if storedName == "" {
		storedName = toolkitType
	}
	cleaned := runtimeNameSanitizer.ReplaceAllString(storedName, "")
	return strings.ReplaceAll(cleaned, ".", "_")
}
