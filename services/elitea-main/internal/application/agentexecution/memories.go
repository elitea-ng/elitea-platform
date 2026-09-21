package agentexecution

import (
	"context"
	"encoding/json"
	"strings"
)

// CurrentMemoryRecall is what one turn's recall lookup answers: the text (if
// any) to splice onto the turn's instructions, and how many of the caller's
// persistent, cross-conversation memories (#870, tenant/0136) it drew from.
// Count is carried separately from Text (rather than the caller counting
// bullet points back out of it) because Count also drives the web's "Using N
// memories" indicator, via RecordCurrentMemoryUsage below — a value that
// must survive independent of whatever prose shape Text ends up in.
type CurrentMemoryRecall struct {
	Text  string
	Count int
	// IDs are the recalled entries' own ids, carried for callers that want
	// to record which memories a turn actually used (this service does not
	// use them beyond the count today).
	IDs []string
}

// CurrentMemoryRecallResolver is the ONE mechanism both StartCurrentApplication
// and StartCurrentAdhoc use to pull a caller's long-term memories into a
// turn — see internal/infra/db/repos.MemoriesRepo for the implementation
// (raw SQL over p_<project>.personal_memory_entries) and its own comments
// for why keyword overlap + recency, not an embedding index, decide which
// memories are recalled.
//
// THIS IS DELIBERATELY NOT THE SAME PATH AS project_context
// (promptcontextreads/handler.go, configuration WHERE type='project_context'):
// project_context is read-only display data whose only runtime effect today
// is an admission-time REFUSAL gate (ResolveCurrentApplicationTurn /
// ResolveCurrentAdhocTurn 422 the turn outright when it is set) — it is
// never woven into a prompt. Memory recall is modeled instead on the
// PERSONA / `default_instructions` mechanism (conversation.meta ->>
// 'default_instructions', concatenated onto `conversation.instructions` by
// ResolveCurrentAdhocTurn's own SQL): ordinary text, appended onto the same
// `instructions` string both the Python and the Rust worker already decode
// out of the `application` field with zero worker-specific handling. A new
// field on the shared AgentExecutionInputV1 proto was considered and
// rejected: both workers verify the decoded message re-encodes to
// byte-identical canonical bytes (protocol/agent.py, agents/protocol.rs),
// so any new field needs lockstep codegen in three languages, and the Rust
// native "ordinary profile" fast path (assembly.rs) refuses outright any
// turn whose `meta` is non-empty — stuffing memory text into `meta` would
// silently kick every memory-bearing turn off that fast path.
type CurrentMemoryRecallResolver interface {
	ResolveCurrentMemoryRecall(ctx context.Context, projectID, actorUserID int64, userInput string) (CurrentMemoryRecall, error)
	// RecordCurrentMemoryUsage is called AFTER a turn has been admitted
	// (never before — see start.go/adhoc.go's own call sites), to stamp
	// `{"memories_used": N}` onto the streaming response message's own
	// meta. Errors are logged and otherwise ignored by every caller in this
	// package: a failed stamp must never be reported as a failed turn, and
	// must never be retried against an admission that has already
	// committed.
	RecordCurrentMemoryUsage(ctx context.Context, projectID int64, responseMessageID string, count int) error
}

// WithMemories attaches the long-term-memory recall resolver (#870) to an
// already-constructed service. A "With" setter rather than a new
// NewCurrentApplicationStartService parameter — same idiom
// v2folders.Handler.WithPool already uses in this codebase — so every
// existing constructor call site (composition.go, and all six
// *_test.go files under this package) keeps working unchanged; a service
// nobody ever calls WithMemories on simply injects no memories, exactly its
// pre-#870 behavior.
func (service *CurrentApplicationStartService) WithMemories(memories CurrentMemoryRecallResolver) *CurrentApplicationStartService {
	service.memories = memories
	return service
}

// resolveCurrentMemoryRecall degrades to "no memories" on any error or
// absent resolver, the same fail-open discipline
// resolveNextInputSuggestionPolicy uses for the OTHER optional per-turn
// enrichment in this file's sibling start.go: a memory-store hiccup must
// cost the user their recalled context for this one turn, not the turn
// itself.
func (service *CurrentApplicationStartService) resolveCurrentMemoryRecall(
	ctx context.Context,
	projectID, actorUserID int64,
	userInput string,
) CurrentMemoryRecall {
	if service.memories == nil {
		return CurrentMemoryRecall{}
	}
	recall, err := service.memories.ResolveCurrentMemoryRecall(ctx, projectID, actorUserID, userInput)
	if err != nil {
		return CurrentMemoryRecall{}
	}
	return recall
}

// recordCurrentMemoryUsage is the post-admission half of recall (see
// CurrentMemoryRecallResolver's own comment on RecordCurrentMemoryUsage).
// Swallows its own error deliberately: by the time this runs, the turn is
// already admitted and streaming, and there is no user-facing failure mode
// left to report it through — only the UI's optional "Using N memories"
// badge is at stake, not the turn.
func (service *CurrentApplicationStartService) recordCurrentMemoryUsage(
	ctx context.Context,
	projectID int64,
	responseMessageID string,
	recall CurrentMemoryRecall,
) {
	if service.memories == nil || recall.Count <= 0 {
		return
	}
	_ = service.memories.RecordCurrentMemoryUsage(ctx, projectID, responseMessageID, recall.Count)
}

// appendCurrentInstructionsMemories concatenates recalled memory text onto
// an existing instructions string, in the SAME "authored text, then a blank
// line, then the extra block" position ResolveCurrentAdhocTurn's own SQL
// already uses for `default_instructions`:
//
//	instructions || E'\n\n' || (conversation.meta ->> 'default_instructions')
//
// Empty memoryText is a no-op (returns instructions unchanged), so a caller
// with no recalled memories never pays for the concatenation or gains a
// trailing blank block.
func appendCurrentInstructionsMemories(instructions, memoryText string) string {
	if memoryText == "" {
		return instructions
	}
	if instructions == "" {
		return memoryText
	}
	return instructions + "\n\n" + memoryText
}

// appendCurrentApplicationMemories splices memoryText onto a frozen
// application version's `instructions` field WITHOUT disturbing any other
// key's exact JSON encoding — it decodes into map[string]json.RawMessage
// rather than map[string]any specifically so number precision (step limits,
// ids) on every OTHER field survives an unrelated re-encode untouched, the
// same discipline promptcontextreads/handler.go's
// parseCurrentProjectContextData uses for the same reason.
//
// Degrades to returning versionDetails UNCHANGED on any decode failure —
// this runs on the turn-start hot path, and a malformed version_details
// document is an existing-agent problem this function must not turn into a
// NEW turn failure.
func appendCurrentApplicationMemories(versionDetails json.RawMessage, memoryText string) json.RawMessage {
	if memoryText == "" || len(versionDetails) == 0 {
		return versionDetails
	}
	// A PIPELINE'S `instructions` IS NOT PROSE — IT IS THE GRAPH (#971).
	//
	// For a direct agent, `instructions` is the free text both workers fold
	// into the system prompt, and appending a block to it is the whole design
	// of this function. For a pipeline it is the YAML DOCUMENT the runtime
	// compiles: services/elitea-worker-rust/src/agents/pipeline.rs calls
	// `PipelineDefinition::from_yaml(shell.instructions())` on exactly this
	// string. Appending anything to it produces a document that is no longer
	// the graph, and the run dies at assembly with
	// `native_agent.invalid_input` — after admission, so the turn is stored as
	// an assistant row flagged `is_error` with EMPTY content and the user is
	// shown a failed run with nothing to read.
	//
	// MEASURED, on the standalone stack and on BOTH worker legs: a project
	// with an enabled, non-empty Project Context could not run ANY pipeline,
	// and emptying the context made the same pipeline pass. That is the same
	// shape #946 was created to end — a project setting that silently disables
	// the project's own execution — reappearing through #946's own injection.
	//
	// So the splice is SKIPPED for a pipeline rather than attempted. The rule
	// lives in this one primitive, and not at the three call sites, because it
	// is a property of the FIELD: every caller that appends to `instructions`
	// needs it, including `appendCurrentApplicationProjectContext`, which
	// delegates here precisely so there is one splice and not two.
	//
	// `skills.go` already draws the same line for the same reason — its
	// instruction-marker pass runs only `if version["agent_type"] !=
	// "pipeline"` — so this is the established rule, not a new exception.
	//
	// WHAT A PIPELINE THEREFORE DOES NOT GET, stated rather than hidden: the
	// project's context does not reach a pipeline run at all. Giving it one
	// would mean writing the text into the graph's LLM nodes, which means
	// re-deriving the pipeline grammar in Go beside the compiler that owns it —
	// a second copy that would be wrong the first time either side changed.
	// The runtime has no per-run context slot to put it in today; #971 records
	// that as the limit, and a real fix belongs in the runtime's own input.
	if currentApplicationInstructionsAreAGraph(versionDetails) {
		return versionDetails
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(versionDetails, &fields); err != nil {
		return versionDetails
	}
	existing := ""
	if raw, ok := fields["instructions"]; ok {
		_ = json.Unmarshal(raw, &existing)
	}
	encoded, err := json.Marshal(appendCurrentInstructionsMemories(existing, memoryText))
	if err != nil {
		return versionDetails
	}
	fields["instructions"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return versionDetails
	}
	return out
}

// currentMemoryRecallUserInputText is the plain text a keyword recall
// resolver matches against — the same bounded input both start paths already
// validate (validCurrentAgentText, maxCurrentAgentUserInputBytes), trimmed
// only, not skill-stripped: recall runs BEFORE skill processing, matching by
// what the user actually typed.
func currentMemoryRecallUserInputText(userInput string) string {
	return strings.TrimSpace(userInput)
}

// currentApplicationInstructionsAreAGraph reports whether this frozen version's
// `instructions` field holds a pipeline DOCUMENT rather than prose — i.e.
// whether `agent_type` is `pipeline`.
//
// Read off the version document the turn is actually running, the way
// `currentProjectContextIgnored` reads its own flag, so a version edited
// mid-conversation is judged by what this turn carries.
//
// A document that cannot be decoded, or that names no `agent_type`, answers
// FALSE: the splice callers already return such a document untouched, and
// answering true here would quietly strip memories and project context from
// every agent whose stored version this function could not read.
func currentApplicationInstructionsAreAGraph(versionDetails json.RawMessage) bool {
	if len(versionDetails) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(versionDetails, &fields); err != nil {
		return false
	}
	raw, found := fields["agent_type"]
	if !found {
		return false
	}
	var agentType string
	if err := json.Unmarshal(raw, &agentType); err != nil {
		return false
	}
	return agentType == currentApplicationPipelineAgentType
}

// currentApplicationPipelineAgentType is the one `agent_type` whose
// `instructions` is a compiled document. It is the same literal
// `skills.go` tests for and the same one the runtime names.
const currentApplicationPipelineAgentType = "pipeline"
