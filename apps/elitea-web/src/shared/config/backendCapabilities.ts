/**
 * Which optional backend surfaces this platform serves.
 *
 * Every capability here is now SERVED except `llmPredictStreaming`, whose
 * transport (a socket.io `application_predict` event) does not exist in this
 * stack at all. The module stays because the rule it enforces still applies:
 * an affordance whose endpoint is not mounted produces a failure the user
 * cannot repair, so a capability is turned on in the same change that mounts
 * its routes — and off only for a MISSING SURFACE, never for a deployment
 * condition an administrator can change.
 *
 * Three entries have LEFT the unserved list. `predict_llm` is served, but
 * only in its blocking mode — which is why the single `aiGeneration` flag
 * that once covered it had to be split (see the three capabilities below).
 * The three DRAFT endpoints are served too, as of #254 P1. And
 * `pipelineTriggers` is served as of #899 — see its own paragraph for what
 * the flag had actually been pinned against.
 */

/** One optional backend surface. */
export type BackendCapability =
  | 'aiGeneration'
  | 'deepwiki'
  | 'inventory'
  | 'llmPredictBlocking'
  | 'llmPredictStreaming'
  | 'pipelineTriggers';

/**
 * What this build serves.
 *
 * `aiGeneration` covers the three DRAFT endpoints only — the agent draft, the
 * project-context draft and the skill draft. All three are served by
 * `services/elitea-main/internal/api/v2/drafts` and described in v2.yaml
 * (`generateApplicationDraft`, `generateProjectContextDraft`,
 * `generateSkillDraft`), so the flag is ON.
 *
 * It stays a capability rather than becoming an unconditional affordance for
 * the same reason `llmPredictBlocking` does: the routes need an LLM gateway.
 * Without one they answer 503 naming LLM_GATEWAY_URL — which the user reads
 * as a deployment fact they can escalate, not as a broken button. That is a
 * deployment condition, not a missing surface, so it does not turn the flag
 * back off.
 *
 * The other two both name `POST /elitea_core/predict_llm/prompt_lib/{id}`, and
 * they are separate flags because the backend serves ONE of its two modes:
 *
 *  - `llmPredictBlocking` — `await_task_timeout: 60`. The generated text comes
 *    back in the HTTP response. Served. Senders: the agent "Edit with AI"
 *    affordance (`features/agents/model/useAiEditAvailability.ts`) and the
 *    canvas mermaid quick-fix (`features/chat-messages/model/useMermaidQuickFix.ts`).
 *  - `llmPredictStreaming` — `await_task_timeout: 0`. The server starts a task
 *    and streams its output over an `application_predict` socket.io event. That
 *    transport does not exist in the Go stack at all, so this stays off no
 *    matter what the route answers. Senders: the pipeline AI assistant
 *    (`features/pipelines/ui/AIAssistantInput.tsx`,
 *    `.../settings/SimpleLLMInputItem.tsx`) and the skill test run
 *    (`pages/skills/EditSkill.tsx` via `features/skills/api/skillsApi.ts`).
 *
 * Collapsing these two back into one flag would light up an affordance whose
 * transport is missing, which is the same "affordance the user cannot repair"
 * this module exists to prevent.
 *
 * `pipelineTriggers` covers the pipeline editor's Schedule and Webhook
 * entry points, and is ON as of #899 — `/pipeline_schedules/...` and
 * `/pipeline_triggers/...` are served by
 * `internal/api/v2/pipelinetriggers` and declared in v2.yaml. The flag
 * was pinned off against a DIFFERENT, deleted route
 * (`/elitea_core/pipeline_trigger/.../trigger`, pylon's single-resource
 * shape); the client that called it is gone, and this app now speaks the
 * two Go facilities through the generated client.
 *
 * The SETTINGS routes do not need the execution runtime — only STARTING a
 * run does — so a deployment with `runtime.enabled` off still configures a
 * schedule or a webhook, and the inbound POST answers 503 saying why
 * (`cmd/elitea-main/main.go`, the same #899 change). That is a deployment
 * fact a user's administrator can change, not a missing surface, so it
 * does not turn the flag back off — the `llmPredictBlocking` distinction
 * this module already makes.
 *
 * `deepwiki` gated the native wiki feature while it was being built, and is
 * now ON. It was off for a reason this module had not had before: the routes it
 * needs were served and the feature still could not work end to end, because
 * the PROVIDER wrote wiki content through a path family elitea-main serves no
 * route in (parity/notes/deepwiki-artifact-store.md, issue #665). Turning it on
 * then would have produced a wiki browser that listed nothing — exactly the
 * affordance-the-user-cannot-repair this module exists to prevent. #665 fixed
 * the provider's client to write where this feature reads, and the flag
 * followed it.
 *
 * WHAT THIS FLAG DOES NOT GATE. Generating a wiki and asking questions about
 * one need the provider SERVICE to be reachable over the facade's mTLS hop. A
 * deployment without one browses and reads wikis and cannot generate, and the
 * generation surface reports that. It is a deployment fact, not a capability —
 * the same distinction as `llmPredictBlocking` above.
 *
 * `inventory` gates the native Inventory screens, and is ON in the same change
 * that mounts `/inventory` and `/inventory/$toolkitId` — the rule this module
 * states at the top.
 *
 * IT IS ON, AND NOT "ON WHEN THE PROVIDER IS THERE", which is the distinction
 * this module keeps making. Every Inventory screen reads through the facade,
 * so a deployment that sets no `ELITEA_INVENTORY_ENABLED` answers 503 to each
 * of them and the screens report the provider's own refusal. That is a
 * deployment fact a user's administrator can change. Hiding the routes for it
 * would instead make the feature unreachable with nothing on screen saying
 * why — the failure this module exists to prevent, in the other direction.
 */
const SERVED: Readonly<Record<BackendCapability, boolean>> = {
  aiGeneration: true,
  deepwiki: true,
  inventory: true,
  llmPredictBlocking: true,
  llmPredictStreaming: false,
  pipelineTriggers: true,
};

/**
 * Test-only overrides.
 *
 * A component test of a hidden affordance has to render it to test it. This
 * app forbids `vi.mock` (`elitea/no-vi-mock`), so the override is an explicit
 * setter, the same shape `get-config.ts` uses for its memo reset. Neither the
 * setter nor this variable reaches the public barrel.
 */
let testOverrides: Partial<Record<BackendCapability, boolean>> = {};

/** Reports whether this build serves one optional backend surface. */
export function hasBackendCapability(name: BackendCapability): boolean {
  return testOverrides[name] ?? SERVED[name];
}

/** Test-only. Kept off the public surface (see index.ts). */
export function setBackendCapabilityForTests(name: BackendCapability, served: boolean): void {
  testOverrides = { ...testOverrides, [name]: served };
}

/** Test-only. Restores every capability to what this build actually serves. */
export function resetBackendCapabilitiesForTests(): void {
  testOverrides = {};
}
