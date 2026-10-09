// Package guardrails is the platform-wide toolkit security policy — the admin
// Configuration page's `guardrails` section, whose five fields address
// `toolkit_security.*`.
//
// # Why the matching rules live here and nowhere else
//
// There are two reference implementations of these checks and they DISAGREE:
//
//   - pylon's `legacy/plugins/elitea_core/utils/toolkit_security.py` compares
//     with a plain `.lower()`;
//   - the SDK's `elitea-sdk/elitea_sdk/runtime/toolkits/security.py` compares on
//     a canonical key — lowercased with every non-alphanumeric character
//     stripped — and additionally reduces routing prefixes off tool names.
//
// This package ports the SDK's rules, because the SDK's are the ones that run at
// tool-call time inside the worker. A Go side that matched more loosely than the
// worker would admit a tool into the catalogue, into a saved agent and into the
// frozen execution input, and only then have it refused (or not) somewhere the
// operator cannot see. One question must have one answer, and the answer has to
// be the strictest of the two.
//
// `policy_parity_test.go` asserts that equality case by case, against the cases
// in `elitea-sdk/tests/runtime/test_blocked_tools.py`, so the two stay equal
// rather than being assumed equal.
//
// # Where the wildcard applies, and where it does not
//
// The SDK supports a `"*"` toolkit key in `sensitive_tools` and NOT in
// `blocked_tools`: `find_sensitive_tool_match` falls back to `_sensitive_tools['*']`,
// while `is_tool_blocked` only ever looks up the exact canonical toolkit key.
// That asymmetry is preserved here deliberately. The shipped compose files set
// `ELITEA_SENSITIVE_TOOLS='{"*":["delete_file"]}'`, so the sensitive wildcard is
// load-bearing; inventing a blocked-tools wildcard the worker does not honour
// would block in the catalogue what still executes at runtime.
package guardrails

import (
	"sort"
	"strings"
	"unicode"
)

// wildcardToolkitKey matches any toolkit. Honoured for sensitive tools only —
// see the package comment.
const wildcardToolkitKey = "*"

// DefaultSensitiveActionCompanyName and DefaultSensitiveActionMessageTemplate
// mirror the SDK's module-level defaults verbatim
// (elitea_sdk/runtime/toolkits/security.py). They are restated rather than
// imported because the worker applies them when the policy leaves them empty,
// and a Go side that sent a different default would change the dialog copy an
// operator sees without anyone having edited it.
const (
	DefaultSensitiveActionCompanyName = "Your organization"

	DefaultSensitiveActionMessageTemplate = "{company_name} requires approval before running the sensitive action '{action_name}'."
)

// CanonicalKey is the SDK's `canonical_match_key`: lowercase, then drop every
// character that is not an ASCII letter or digit.
//
// So `CreateFile`, `create_file`, `create-file` and `Create File` all collapse to
// `createfile`. This key is used EXCLUSIVELY for membership tests — tools are
// still invoked and displayed under their natural names.
//
// A value that canonicalises to the empty string (`"---"`, `"  "`) is not a key;
// every caller drops it rather than storing an entry that can never match
// anything meaningful.
func CanonicalKey(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r < unicode.MaxASCII && (unicode.IsLower(r) || unicode.IsDigit(r)) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// canonicalToolkitKey is CanonicalKey with the `"*"` wildcard preserved, which
// CanonicalKey would otherwise strip to "".
func canonicalToolkitKey(value string) string {
	if strings.TrimSpace(value) == wildcardToolkitKey {
		return wildcardToolkitKey
	}
	return CanonicalKey(value)
}

// ToolNameAliases is the SDK's `get_tool_name_aliases`: the successive
// reductions of a runtime tool name, longest first.
//
// A tool arrives under one of three shapes depending on how it was routed — the
// base name (`list_branches_in_repo`), the legacy prefixed name
// (`github___list_branches_in_repo`), or the runtime-prefixed name
// (`elitea_core:list_branches_in_repo`) — and a guardrail that treated those as
// three different actions would be trivially bypassed by whichever route the
// operator did not name.
//
// The result is lowercased but NOT canonicalised; callers canonicalise each
// alias themselves, matching the SDK.
func ToolNameAliases(toolName string) []string {
	normalized := strings.ToLower(strings.TrimSpace(toolName))
	if normalized == "" {
		return nil
	}

	var aliases []string
	current := normalized
	for current != "" && !containsString(aliases, current) {
		aliases = append(aliases, current)

		reduced := current
		if _, after, found := strings.Cut(reduced, "___"); found {
			reduced = strings.TrimSpace(after)
		}
		if _, after, found := strings.Cut(reduced, ":"); found {
			reduced = strings.TrimSpace(after)
		}
		if reduced == "" || reduced == current {
			break
		}
		current = reduced
	}
	return aliases
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Policy is the resolved guardrails configuration.
//
// Every field is already canonicalised at construction, so a Policy is
// comparison-ready and the raw admin values are never consulted at match time.
// The zero Policy blocks nothing and marks nothing sensitive, which is what an
// unreadable configuration store must degrade to — see NewPolicy's callers.
type Policy struct {
	blockedToolkits map[string]struct{}
	blockedTools    map[string]map[string]struct{}
	sensitiveTools  map[string]map[string]struct{}

	// companyName and messageTemplate are carried verbatim, NOT canonicalised:
	// they are display copy, not match keys.
	companyName     string
	messageTemplate string
}

// PolicyInput is the raw admin-stored shape, exactly as
// `centry.platform_config` holds it.
type PolicyInput struct {
	BlockedToolkits []string
	BlockedTools    map[string][]string
	SensitiveTools  map[string][]string
	CompanyName     string
	MessageTemplate string
}

// NewPolicy canonicalises raw admin values into a Policy.
//
// Entries that canonicalise to the empty string are dropped rather than stored,
// which is the SDK's `_normalize_tools_mapping` behaviour and matters for a
// reason beyond tidiness: an empty key would be produced by a separator-only
// value like `"---"`, and an empty key that matched an empty toolkit name would
// silently widen the blocklist to things the operator never named.
func NewPolicy(input PolicyInput) Policy {
	policy := Policy{
		blockedToolkits: make(map[string]struct{}, len(input.BlockedToolkits)),
		blockedTools:    normalizeToolMapping(input.BlockedTools),
		sensitiveTools:  normalizeToolMapping(input.SensitiveTools),
		companyName:     strings.TrimSpace(input.CompanyName),
		messageTemplate: strings.TrimSpace(input.MessageTemplate),
	}
	for _, raw := range input.BlockedToolkits {
		if key := CanonicalKey(raw); key != "" {
			policy.blockedToolkits[key] = struct{}{}
		}
	}
	return policy
}

func normalizeToolMapping(mapping map[string][]string) map[string]map[string]struct{} {
	normalized := make(map[string]map[string]struct{}, len(mapping))
	for rawToolkit, rawTools := range mapping {
		toolkitKey := canonicalToolkitKey(rawToolkit)
		if toolkitKey == "" {
			continue
		}
		tools := make(map[string]struct{}, len(rawTools))
		for _, rawTool := range rawTools {
			if key := CanonicalKey(rawTool); key != "" {
				tools[key] = struct{}{}
			}
		}
		// An entry with no usable tool names is still recorded. It is how the
		// operator's intent survives a round trip through the form, and an empty
		// set matches nothing, so recording it is free.
		normalized[toolkitKey] = tools
	}
	return normalized
}

// Empty reports whether this policy would refuse nothing at all. Callers on hot
// paths use it to skip the walk entirely.
func (p Policy) Empty() bool {
	return len(p.blockedToolkits) == 0 && len(p.blockedTools) == 0 && len(p.sensitiveTools) == 0
}

// ToolkitBlocked reports whether an entire toolkit TYPE is disabled.
func (p Policy) ToolkitBlocked(toolkitType string) bool {
	key := CanonicalKey(toolkitType)
	if key == "" {
		return false
	}
	_, blocked := p.blockedToolkits[key]
	return blocked
}

// ToolBlocked reports whether one tool may not be used.
//
// A blocked TOOLKIT blocks every tool in it, which is the SDK's ordering and the
// reason a caller need only ask this one question per tool.
//
// Matching is toolkit-scoped: `create_file` blocked under `github` leaves
// `artifacts`' identically-named tool alone. Two toolkits sharing a common verb
// is the normal case, and a global match would silently disable far more than
// the operator selected.
func (p Policy) ToolBlocked(toolkitType, toolName string) bool {
	if p.ToolkitBlocked(toolkitType) {
		return true
	}
	tools, ok := p.blockedTools[CanonicalKey(toolkitType)]
	if !ok || len(tools) == 0 {
		return false
	}
	for _, alias := range ToolNameAliases(toolName) {
		if _, blocked := tools[CanonicalKey(alias)]; blocked {
			return true
		}
	}
	return false
}

// BlockedToolkits returns the canonical keys of every blocked toolkit type, for
// callers that report rather than test. Order is unspecified.
func (p Policy) BlockedToolkits() []string {
	keys := make([]string, 0, len(p.blockedToolkits))
	for key := range p.blockedToolkits {
		keys = append(keys, key)
	}
	return keys
}

// SensitiveMatch reports which configured toolkit identity marked a tool
// sensitive, and whether one did.
//
// `toolkitIdentifiers` is the ordered list of names the tool could be configured
// under — its type and its instance name — because the admin form is populated
// from toolkit TYPES while a running tool knows its instance name too. The
// wildcard `"*"` is consulted last, after every concrete identifier, so a
// specific entry always decides ahead of the catch-all.
func (p Policy) SensitiveMatch(toolName string, toolkitIdentifiers ...string) (string, bool) {
	aliases := ToolNameAliases(toolName)
	if len(aliases) == 0 || len(p.sensitiveTools) == 0 {
		return "", false
	}

	candidates := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		if key := CanonicalKey(alias); key != "" {
			candidates[key] = struct{}{}
		}
	}

	seen := make(map[string]struct{}, len(toolkitIdentifiers))
	for _, identifier := range toolkitIdentifiers {
		key := CanonicalKey(identifier)
		if key == "" {
			continue
		}
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		if intersects(p.sensitiveTools[key], candidates) {
			return key, true
		}
	}

	if intersects(p.sensitiveTools[wildcardToolkitKey], candidates) {
		return wildcardToolkitKey, true
	}
	return "", false
}

func intersects(configured map[string]struct{}, candidates map[string]struct{}) bool {
	for candidate := range candidates {
		if _, ok := configured[candidate]; ok {
			return true
		}
	}
	return false
}

// CompanyName and MessageTemplate return the dialog copy, falling back to the
// SDK's own defaults when the operator left the field blank — so an unset field
// produces the SDK's wording rather than an empty string interpolated into the
// authorisation dialog.
func (p Policy) CompanyName() string {
	if p.companyName == "" {
		return DefaultSensitiveActionCompanyName
	}
	return p.companyName
}

func (p Policy) MessageTemplate() string {
	if p.messageTemplate == "" {
		return DefaultSensitiveActionMessageTemplate
	}
	return p.messageTemplate
}

// SensitiveAction is the approval copy for one sensitive tool call: the SDK's
// `get_sensitive_tool_policy` action name and rendered policy message
// (elitea_sdk/runtime/toolkits/security.py), which the chat HITL card shows as
// `action_label` and `policy_message`.
type SensitiveAction struct {
	MatchedIdentifier string
	ActionLabel       string
	PolicyMessage     string
}

// SensitiveAction answers the approval copy for toolName when the policy marks
// it sensitive under any of toolkitIdentifiers, and false otherwise.
//
// toolkitLabel is what the SDK calls the toolkit in the message (the instance
// name, falling back to the type). The template's placeholders are the SDK's:
// {company_name}, {tool_name}, {toolkit_name}, {toolkit_type},
// {toolkit_label} and {action_name}. An unknown placeholder is left as it is,
// where the SDK's str.format would fall back to the default template; the
// admin form documents only the known ones.
func (p Policy) SensitiveAction(toolName, toolkitLabel string, toolkitIdentifiers ...string) (SensitiveAction, bool) {
	matched, ok := p.SensitiveMatch(toolName, toolkitIdentifiers...)
	if !ok {
		return SensitiveAction{}, false
	}
	label := strings.TrimSpace(toolkitLabel)
	if label == "" {
		label = matched
	}
	if label == "" {
		label = "this toolkit"
	}
	action := label + "." + toolName
	message := strings.NewReplacer(
		"{company_name}", p.CompanyName(),
		"{tool_name}", toolName,
		"{toolkit_name}", label,
		"{toolkit_type}", label,
		"{toolkit_label}", label,
		"{action_name}", action,
	).Replace(p.MessageTemplate())
	return SensitiveAction{MatchedIdentifier: matched, ActionLabel: action, PolicyMessage: message}, true
}

// RuntimePolicy is the projection handed to the worker in the agent execution
// input, and it is deliberately a separate type from Policy.
//
// Policy is a matcher: its maps are sets, keyed for lookup. This is a document,
// ordered and JSON-shaped, because it crosses a process boundary into Python and
// is compared byte-for-byte by the admission path's idempotency. Sorting every
// list and marshalling through a struct is what stops two identical policies
// producing two different payloads for the same turn.
//
// The field names are the SDK's keyword arguments
// (`elitea_sdk.runtime.toolkits.security.configure_sensitive_tools` /
// `configure_blocklist`), so the worker maps them across without a translation
// table that could drift.
type RuntimePolicy struct {
	BlockedToolkits []string            `json:"blocked_toolkits"`
	BlockedTools    map[string][]string `json:"blocked_tools"`
	SensitiveTools  map[string][]string `json:"sensitive_tools"`
	CompanyName     string              `json:"sensitive_action_company_name"`
	MessageTemplate string              `json:"sensitive_action_message_template"`
}

// Runtime projects the policy for transport.
//
// CANONICAL keys are sent, not the operator's raw strings. The worker
// canonicalises whatever it receives through the same rule, and canonicalising a
// canonical key is a no-op, so this is lossless for matching and removes a class
// of disagreement: the two sides cannot normalise differently if only one of them
// ever normalises. The `"*"` wildcard survives because canonicalToolkitKey
// preserves it.
//
// The blocklist travels too, even though the freeze has already stripped blocked
// toolkits and tools out of this execution's input. That is defence in depth
// rather than duplication: the freeze can only remove what the saved agent
// NAMES, and a tool that reaches the graph another way — discovered from an MCP
// server at runtime, or surfaced by a nested toolkit — was never in the frozen
// list to be removed. The worker's own filter is the backstop for those.
//
// The dialog copy is sent RAW. It is display text, not a match key, and
// canonicalising it would strip the punctuation and placeholder braces the
// template is made of.
func (p Policy) Runtime() RuntimePolicy {
	blockedToolkits := p.BlockedToolkits()
	sort.Strings(blockedToolkits)
	return RuntimePolicy{
		BlockedToolkits: blockedToolkits,
		BlockedTools:    sortedToolMapping(p.blockedTools),
		SensitiveTools:  sortedToolMapping(p.sensitiveTools),
		CompanyName:     p.CompanyName(),
		MessageTemplate: p.MessageTemplate(),
	}
}

func sortedToolMapping(mapping map[string]map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(mapping))
	for toolkit, tools := range mapping {
		names := make([]string, 0, len(tools))
		for name := range tools {
			names = append(names, name)
		}
		sort.Strings(names)
		out[toolkit] = names
	}
	return out
}
