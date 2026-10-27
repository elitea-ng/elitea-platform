/**
 * The reviewed create-application body for a generated draft (ADR-0037
 * "Save and availability"): the same `POST /elitea_core/applications`
 * request the web review form sends (`useAgentDraftApproval`), for a client
 * without that form (mobile). Nothing new is saved: it is an ordinary
 * `agent` application with one version.
 */
import type { StreamedAgentDraft } from './draftStream';

/** The first version's name, as every create path names it. */
export const BASE_VERSION_NAME = 'base';
/** A generated agent's step limit, as the web review form sets it. */
export const GENERATED_AGENT_STEP_LIMIT = 25;

export interface AgentCreateModel {
  readonly name: string;
  readonly project_id?: number | undefined;
}

export interface AgentCreateInput {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly welcome_message?: string;
  readonly conversation_starters?: readonly string[];
}

/** The create body. Without a model, the platform's default model stands, as on the web. */
export function agentCreateBody(draft: AgentCreateInput | StreamedAgentDraft, model?: AgentCreateModel) {
  const starters = (draft.conversation_starters ?? []).map((starter) => starter.trim()).filter((starter) => starter !== '');
  const welcome = draft.welcome_message?.trim() ?? '';
  return {
    name: draft.name.trim(),
    description: draft.description.trim(),
    type: 'interface',
    versions: [
      {
        name: BASE_VERSION_NAME,
        instructions: draft.instructions,
        ...(welcome === '' ? {} : { welcome_message: welcome }),
        conversation_starters: starters,
        tags: [],
        meta: { step_limit: GENERATED_AGENT_STEP_LIMIT, internal_tools: [] as string[] },
        ...(model === undefined
          ? {}
          : { llm_settings: { model_name: model.name, ...(model.project_id === undefined ? {} : { model_project_id: model.project_id }) } }),
      },
    ],
  };
}
