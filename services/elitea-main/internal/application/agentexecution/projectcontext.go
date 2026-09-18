package agentexecution

import (
	"context"
	"encoding/json"
	"strings"
)

// Project Context INJECTION (#946).
//
// A project carries ONE `project_context` row (tenant `configuration`, type
// 'project_context', written by Settings > Project Context and by the
// `project_context_builder` chat module). Until now its only runtime effect
// was an admission-time REFUSAL: every resolve/insert variant in
// internal/db/queries/agent_chat.sql carried a
// `NOT EXISTS (… type = 'project_context' AND enabled AND content <> '')`
// clause, so the moment a project had a context the resolver returned zero
// rows and EVERY send in that project answered 422
// unsupported_agent_execution — including the send that would have turned the
// context back off. The gate existed because injection was unimplemented and
// running a turn that silently ignored the project's context was judged worse
// than refusing it; the result was a feature that disabled the project's chat.
//
// The gate is gone and this file is what replaced it.
//
// HOW THE TEXT REACHES THE MODEL, AND WHY THERE IS NO NEW WIRE FIELD. Exactly
// the way recalled memory text does (memories.go's CurrentMemoryRecallResolver
// comment carries the full argument, which applies here unchanged): the
// content is spliced onto the turn's `instructions` — `version_details.
// instructions` for a selected application, `application.instructions` for an
// adhoc turn — which BOTH workers already fold into the system prompt with
// zero worker-specific handling (the native runtime at
// services/elitea-worker-rust/src/agents/assembly.rs reads `instructions` off
// the frozen version and hands it over as `system_instruction`). A new field
// on AgentExecutionInputV1 would need lockstep codegen in three languages and
// both workers re-verify that the decoded message re-encodes to byte-identical
// canonical bytes, and stuffing the text into `meta` would kick every
// context-bearing turn off the Rust native fast path.
//
// WHAT IT DELIBERATELY DOES NOT REACH. A sub-agent. A delegated child resolves
// ITS OWN version's instructions inside the worker (assembly.rs's child
// profile), and this splice only ever touches the turn's own application
// document, so the context stays with the master agent — which is the
// behaviour ELITEA-0952 asks for, obtained by construction rather than by a
// second rule that could drift.

// CurrentProjectContext is the project's single context row as the start path
// needs it: the text and whether the project has it switched on. Both are
// carried (rather than collapsing to "the text to inject, or empty") because
// "enabled with empty content" and "disabled with content" are different user
// states, and only the resolver should decide that neither injects.
type CurrentProjectContext struct {
	Content string
	Enabled bool
}

// InjectableText is the text to weave into a turn, or "" when this project's
// context must not reach a prompt. A DISABLED context injects nothing even
// when its content is still stored — the toggle is the user's own "stop
// sending this", asserted directly by ELITEA-0951's second half — and an
// enabled-but-blank context injects nothing rather than an empty block.
func (context CurrentProjectContext) InjectableText() string {
	if !context.Enabled {
		return ""
	}
	return strings.TrimSpace(context.Content)
}

// CurrentProjectContextResolver reads the project's context row. Implemented
// by internal/infra/db/repos.ProjectContextRepo over the same tenant
// `configuration` row the Settings panel writes
// (internal/api/v2/eliteacore/handler.go's UpdateProjectContext, #888) and the
// read route serves (internal/api/v2/promptcontextreads/handler.go), so what a
// user sees on that screen is what a turn is given — one row, one meaning.
type CurrentProjectContextResolver interface {
	ResolveCurrentProjectContext(ctx context.Context, projectID int64) (CurrentProjectContext, error)
}

// WithProjectContext attaches the project-context resolver, in the same
// after-construction idiom WithMemories uses and for the same reason: every
// existing constructor call site (composition.go and this package's tests)
// keeps working, and a service nobody attaches it to injects no project
// context — the behaviour every unit test that predates this file expects.
func (service *CurrentApplicationStartService) WithProjectContext(
	projectContext CurrentProjectContextResolver,
) *CurrentApplicationStartService {
	service.projectContext = projectContext
	return service
}

// resolveCurrentProjectContextText answers the text to inject for one turn, or
// "" for every reason there is not to inject one: no resolver attached, a read
// that failed, a disabled toggle, blank content.
//
// FAILS OPEN, exactly like resolveCurrentMemoryRecall. This is the whole point
// of #946: the previous design failed CLOSED on the mere presence of a context
// row and took the project's chat with it. A configuration read that hiccups
// must cost this one turn its project context, never the turn.
func (service *CurrentApplicationStartService) resolveCurrentProjectContextText(
	ctx context.Context,
	projectID int64,
) string {
	if service.projectContext == nil {
		return ""
	}
	resolved, err := service.projectContext.ResolveCurrentProjectContext(ctx, projectID)
	if err != nil {
		return ""
	}
	return resolved.InjectableText()
}

// currentProjectContextBlockOpen / Close delimit the injected text inside the
// system prompt. A named, closed block rather than bare prose: the model has
// to be able to tell the project's standing context apart from the agent's own
// instructions, and a reader of a journaled prompt has to be able to see where
// it came from. The tag names the feature the user switched on, so the two
// screens and the prompt all say "project context".
const (
	currentProjectContextBlockOpen  = "<project_context>"
	currentProjectContextBlockClose = "</project_context>"
)

// maxCurrentProjectContextInstructionsBytes is the ceiling the COMBINED
// instructions (the agent's own, plus recalled memories, plus this block) must
// stay under for the injection to happen at all.
//
// It exists because the native runtime REFUSES an over-long instruction string
// rather than trimming it: `bounded_instruction` /
// `bounded_adhoc_instruction` (services/elitea-worker-rust/src/agents/
// assembly.rs) cap it at 64 KiB and answer `invalid_profile` past that, which
// would fail the turn. A project context can legitimately be large — the
// builder tool accepts 48 KiB (RuntimeProjectContextWriteRequest,
// internal/infra/storage/runtime_entity_builder.go), well past what the
// settings screen's own 2500-character editor can produce — so "agent with
// long instructions + long context" is reachable, and injecting it blindly
// would re-create the exact failure mode #946 is about: a project context that
// makes the project's chat stop working.
//
// So an injection that would not fit is SKIPPED, not truncated and not fatal.
// Truncating would hand the model half a context as though it were the whole
// one, and refusing would put the gate back. 60 KiB leaves the runtime's own
// bound room for the rendering the worker does afterwards.
const maxCurrentProjectContextInstructionsBytes = 60 * 1024

// currentProjectContextBlock renders the text as the delimited block. Empty in
// -> empty out, so every caller can pass the resolver's answer straight
// through without first testing it.
func currentProjectContextBlock(text string) string {
	if text == "" {
		return ""
	}
	return currentProjectContextBlockOpen + "\n" + text + "\n" + currentProjectContextBlockClose
}

// currentProjectContextFits reports whether the block can be added to these
// instructions without pushing the result past what the runtime will accept.
// The +2 is the blank line the splice inserts between them.
func currentProjectContextFits(instructions, block string) bool {
	if block == "" {
		return false
	}
	total := len(block)
	if instructions != "" {
		total += len(instructions) + 2
	}
	return total <= maxCurrentProjectContextInstructionsBytes
}

// currentProjectContextIgnored reports whether THIS agent version opts out,
// through the Advanced panel's own key — `version_details.meta.
// ignore_project_context`, the boolean
// apps/elitea-web/src/features/agents/ui/ApplicationAdvanceSettings.tsx writes
// (ELITEA-0945). Read off the frozen version details the turn is actually
// running, not the stored row, so an agent edited mid-conversation opts out
// from the turn that first carries the flag.
//
// Anything that is not literal `true` means "do not ignore": a malformed or
// absent meta must not silently strip a project's context out of its prompts,
// which is a change the user cannot see and would not be told about.
func currentProjectContextIgnored(versionDetails json.RawMessage) bool {
	if len(versionDetails) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(versionDetails, &fields); err != nil {
		return false
	}
	rawMeta, found := fields["meta"]
	if !found {
		return false
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return false
	}
	rawFlag, found := meta["ignore_project_context"]
	if !found {
		return false
	}
	var ignored bool
	if err := json.Unmarshal(rawFlag, &ignored); err != nil {
		return false
	}
	return ignored
}

// appendCurrentApplicationProjectContext splices the project-context block
// onto a frozen application version's `instructions`, and answers the version
// unchanged when the version opts out or there is nothing to inject.
//
// It reuses appendCurrentApplicationMemories (memories.go) rather than
// re-deriving the splice: that function is the package's ONE
// "concatenate a block onto version_details.instructions without disturbing
// any other key's exact JSON encoding" primitive — map[string]json.RawMessage
// so number precision on every other field survives, and a decode failure
// returns the document untouched rather than turning a malformed stored agent
// into a new turn failure. Two copies of that would be two chances to get the
// re-encode discipline wrong.
func appendCurrentApplicationProjectContext(
	versionDetails json.RawMessage,
	projectContextText string,
) json.RawMessage {
	if projectContextText == "" || currentProjectContextIgnored(versionDetails) {
		return versionDetails
	}
	block := currentProjectContextBlock(projectContextText)
	if !currentProjectContextFits(currentApplicationInstructionsText(versionDetails), block) {
		return versionDetails
	}
	return appendCurrentApplicationMemories(versionDetails, block)
}

// currentApplicationInstructionsText reads the instructions already on a
// version document, for the budget check alone. A document it cannot decode
// answers "" — appendCurrentApplicationMemories would return that document
// untouched anyway, so the budget's answer for it cannot matter.
func currentApplicationInstructionsText(versionDetails json.RawMessage) string {
	if len(versionDetails) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(versionDetails, &fields); err != nil {
		return ""
	}
	raw, found := fields["instructions"]
	if !found {
		return ""
	}
	var instructions string
	if err := json.Unmarshal(raw, &instructions); err != nil {
		return ""
	}
	return instructions
}

// appendCurrentInstructionsProjectContext is the adhoc path's equivalent: an
// adhoc turn has a plain instructions string and no version meta, so there is
// no per-agent opt-out to consult — the toggle belongs to a saved agent, and
// an adhoc turn is not one.
func appendCurrentInstructionsProjectContext(instructions, projectContextText string) string {
	if projectContextText == "" {
		return instructions
	}
	block := currentProjectContextBlock(projectContextText)
	if !currentProjectContextFits(instructions, block) {
		return instructions
	}
	return appendCurrentInstructionsMemories(instructions, block)
}
