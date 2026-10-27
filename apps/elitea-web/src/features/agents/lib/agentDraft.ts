/**
 * `AgentDraft` — the shape `GenerateAgentReviewForm`/`GenerateAgentModal`
 * edit and submit, ported from the field set the baseline's
 * `GenerateAgentReviewForm.jsx` (`apps/elitea-ui/src/[fsd]/features/agent/
 * ui/generate-agent-modal/GenerateAgentReviewForm.jsx:20-35`) reads off
 * `draft` (`name`, `description`, `instructions`, `welcome_message`,
 * `conversation_starters`, `suggested_toolkits`, `suggested_mcp`,
 * `suggested_pipelines`, `suggested_agents`, `suggested_skills`).
 *
 * **THE BACKEND GAP THIS FILE USED TO DOCUMENT IS CLOSED (#254 P1).**
 * `POST /elitea_core/generate_application_draft/prompt_lib/{projectId}` is
 * served by `services/elitea-main/internal/api/v2/drafts` and described in
 * `api/openapi/v2.yaml` as `generateApplicationDraft`. It answers with the
 * generated `ApplicationDraft` — `{name, description, instructions,
 * welcome_message, conversation_starters}` — so `readDraft` (client-core)
 * carries the real fields across instead of dropping a chat completion into
 * `instructions` and blanking the rest.
 *
 * **THE `suggested_*` GAP IS ALSO CLOSED (#881).** The endpoint now scores
 * the project's toolkit instances, agents, pipelines and skills against the
 * generated draft (Go-side lexical matching, not an LLM candidate pick —
 * see `internal/api/v2/drafts/suggestions.go`'s own doc comment for why) and
 * returns up to 5 matches per category. `readDraft` copies all
 * five straight through — every id on the wire already resolves to a real
 * project entity, so no client-side re-derivation is needed, same as the
 * text fields above.
 */

/**
 * One AI-suggested resource the review form can offer to attach post-create.
 *
 * The optional fields carry an explicit `| undefined` (not just `?:`) to
 * stay assignable, under `exactOptionalPropertyTypes`, from the generated
 * `SuggestedResource` (`shared/api/generated/model`) — a zod `.optional()`
 * field's inferred type is `T | undefined`, not merely an optional key.
 */
export interface SuggestedResource {
  readonly id: number | string;
  readonly name: string;
  /** Toolkit-only: the toolkit type string (`item.type` in the baseline's `SuggestionItem.jsx`). */
  readonly type?: string | undefined;
  readonly description?: string | undefined;
  /** Agent/pipeline-only: `'pipeline'` marks a suggested application as a pipeline (baseline `a.agent_type === 'pipeline'`). */
  readonly agent_type?: string | undefined;
}

export interface AgentDraft {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly welcome_message: string;
  readonly conversation_starters: readonly string[];
  readonly suggested_toolkits: readonly SuggestedResource[];
  readonly suggested_mcp: readonly SuggestedResource[];
  readonly suggested_pipelines: readonly SuggestedResource[];
  readonly suggested_agents: readonly SuggestedResource[];
  readonly suggested_skills: readonly SuggestedResource[];
}

export const EMPTY_AGENT_DRAFT: AgentDraft = {
  name: '',
  description: '',
  instructions: '',
  welcome_message: '',
  conversation_starters: [],
  suggested_toolkits: [],
  suggested_mcp: [],
  suggested_pipelines: [],
  suggested_agents: [],
  suggested_skills: [],
};

/**
 * `filterEmptyStrings` — the baseline's `common/applicationUtils.js`
 * helper (referenced by `GenerateAgentModal.jsx:240`,
 * `filterEmptyStrings(draftData.conversation_starters)`), not promoted to
 * any `entities/` slice by the Wave-2 promotion pass. Reproduced locally
 * (its entire behaviour is one filter predicate — trimmed, non-blank
 * strings only).
 */
export function filterEmptyStrings(values: readonly string[]): string[] {
  return values.filter((value) => value.trim().length > 0);
}
