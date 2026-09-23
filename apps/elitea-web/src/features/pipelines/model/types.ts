/**
 * `features/pipelines` create-mode value shapes — a plain-object mirror of
 * the baseline's Formik `values` shape for the pipeline create form
 * (`apps/elitea-ui/src/pages/NewChat/PipelineEditor.jsx`'s create-mode
 * branch, which renders `CreateAgentForm` with `entityType="pipeline"`;
 * `CreateAgentForm.jsx`'s own `formik.values.{name,description,
 * version_details.*}` reads apply unchanged — `entityType` only toggles
 * whether `GenerateAgentButton` renders, `CreateAgentForm.jsx:106`).
 *
 * **DISCLOSED REDESIGN — no ambient form-library context**, same rationale
 * as `features/agents/model/types.ts`'s own doc comment (this app has no
 * Formik; `AgentDraftValues`/`AgentFieldChange` established this exact
 * "`values` + `onFieldChange(path, value)` plain-prop" contract first).
 * Duplicated rather than imported (`no-sideways-features`) — `PipelineDraftValues`
 * is a strict SUBSET of `AgentDraftValues` (no `tools`, since the baseline's
 * `CreateAgentForm` never renders anything tools-related for either entity
 * type in create mode — tools are attached post-create).
 */
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';


/** One `version_details.variables[]` entry — same shape as `apps/elitea-ui/src/components/VariableList.jsx:7-24` used for both agents and pipelines. */
interface PipelineVariable {
  readonly id?: string | number | undefined;
  readonly name: string;
  readonly value: string;
}

/** `version_details.meta` — settings bag, same shape as `AgentVersionMeta`. */
interface PipelineVersionMeta {
  readonly step_limit?: number | undefined;
  readonly [metaKey: string]: unknown;
}

/** `version_details` — the mutable half of a pipeline version's create-mode draft. */
interface PipelineVersionDetails {
  readonly instructions?: string | undefined;
  readonly welcome_message?: string | undefined;
  /**
   * #940 A12 — the pipeline's chat starters. Same field, same shape, as
   * `AgentVersionDetails.conversation_starters`: the baseline renders ONE
   * create form for both entity types (`CreateAgentForm entityType="pipeline"`),
   * so a pipeline draft that could not hold them made the shared form's
   * starters panel write into a value the submit could not read.
   */
  readonly conversation_starters?: readonly string[] | undefined;
  readonly tags?: readonly string[] | undefined;
  readonly variables?: readonly PipelineVariable[] | undefined;
  readonly meta?: PipelineVersionMeta | undefined;
  /** The model this version runs on — same field, same shared type, as `AgentVersionDetails.llm_settings`. */
  readonly llm_settings?: AgentLlmSettings | undefined;
}

/** The full `CreateAgentForm`-level draft for a pipeline — `formik.values` at the top level in the baseline. */
export interface PipelineDraftValues {
  readonly id?: number | undefined;
  readonly name?: string | undefined;
  readonly description?: string | undefined;
  readonly version_details?: PipelineVersionDetails | undefined;
}

/** Generic nested-path setter — mirrors `AgentFieldChange`'s signature (Formik's own `setFieldValue(path, value)`). */
export type PipelineFieldChange = (path: string, value: unknown) => void;
