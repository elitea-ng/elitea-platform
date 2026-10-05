import { useQuery } from '@tanstack/react-query';

import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { hasBackendCapability } from '@/shared/config';

import {
  aiEditModelName,
  findServicePrompt,
  getProjectModelCatalogue,
  getServicePromptTypes,
  getServicePrompts,
  servicePromptDefaultsByKey,
} from '../api/aiEdit';

/**
 * THE GATE for the "Edit with AI" affordance.
 *
 * The brief for this port is explicit that the control must not ship in a
 * state where every click errors, and there are three independent ways it
 * would:
 *
 *  1. **The route.** `POST /elitea_core/predict_llm/prompt_lib/{projectId}`
 *     is served in its BLOCKING mode, which is the mode this affordance uses
 *     (#126 removed the group; this is its replacement). The switch is
 *     `hasBackendCapability('llmPredictBlocking')` and NOT the broader
 *     `aiGeneration`, which covers the three DRAFT endpoints — served since
 *     #254 P1, and a different contract: they return a structured draft, not
 *     a completion this affordance can paste into a field. A deployment that
 *     sets no `LLM_GATEWAY_URL` has no LLM to reach, so the flag remains the
 *     honest gate rather than a formality.
 *  2. **The prompt.** The base instruction the model is steered with is a
 *     Service Prompt, read from `/configurations/*`. Those routes only exist
 *     when `ELITEA_CONFIGURATIONS_ENABLED` is on — FALSE in a default
 *     install — so in a default install both queries below fail and no
 *     prompt resolves. An authored project/shared configuration wins; the
 *     type descriptor's `default_by_key` is the fallback. Neither present
 *     means no prompt, which means no affordance.
 *  3. **The model.** `predict_llm` is given an explicit `llm_settings`. Its
 *     model is the project's CURRENT configured default model, read from the
 *     model catalogue; the agent's own `version_details.llm_settings.model_name`
 *     is the fallback, and the catalogue's first model after that. With none
 *     of them, nothing can be asked to generate anything.
 *
 * `isAvailable` is the AND of all three. Nothing here retries: a 404 on the
 * configurations routes is a permanent property of the deployment, not a
 * transient failure, and retrying it on every mount would just be noise.
 */

/** The service-prompt key the agent-instructions edit is steered by. */
const AI_EDIT_INSTRUCTIONS_PROMPT_KEY = 'edit_application_draft';

export interface UseAiEditAvailabilityOptions {
  readonly projectId: string | undefined;
  readonly modelSettings: AgentLlmSettings | null | undefined;
}

export interface UseAiEditAvailabilityResult {
  /** Render the affordance only when this is true. */
  readonly isAvailable: boolean;
  /** The resolved base prompt — empty string when none resolved. */
  readonly basePrompt: string;
  readonly modelName: string;
  readonly isResolving: boolean;
}

const AI_EDIT_QUERY_ROOT = ['agents', 'aiEdit'] as const;

/**
 * THE MODEL is the project's CURRENT configured default (legacy issue 6872).
 * The version's own `model_name` is the default that was current when the
 * agent was created; after an admin changes the project default it is stale,
 * and "Edit with AI" ran on the old model. The version's model is the
 * fallback for a project with no CONFIGURED default, or a catalogue that
 * cannot be read. The catalogue names its first item as the default when
 * none is configured (`default_model_configured: false`); that item wins only
 * when the version has no model either (`aiEditModelName`).
 */
function useAiEditModel(
  projectId: string | undefined,
  modelSettings: AgentLlmSettings | null | undefined,
  enabled: boolean,
): { readonly modelName: string; readonly isLoading: boolean } {
  const catalogue = useQuery({
    queryKey: [...AI_EDIT_QUERY_ROOT, 'defaultModel', projectId],
    queryFn: ({ signal }) => getProjectModelCatalogue(projectId ?? '', signal),
    enabled,
    retry: false,
  });
  const versionModel = modelSettings?.model_name ?? '';
  return { modelName: aiEditModelName(catalogue.data, versionModel), isLoading: catalogue.isLoading };
}

/** True for a usable project id. A function, to keep the hook's own branches in the complexity budget. */
function isProjectId(projectId: string | undefined): projectId is string {
  return projectId !== undefined && projectId !== '';
}

export function useAiEditAvailability(options: UseAiEditAvailabilityOptions): UseAiEditAvailabilityResult {
  const { projectId, modelSettings } = options;

  const canRead = hasBackendCapability('llmPredictBlocking') && isProjectId(projectId);
  const model = useAiEditModel(projectId, modelSettings, canRead);
  const { modelName } = model;

  // The prompt reads wait for a model: a build without the capability, or a
  // project and version with no model at all, must not issue configuration
  // requests it can do nothing with.
  const enabled = canRead && modelName !== '';

  const promptTypes = useQuery({
    queryKey: [...AI_EDIT_QUERY_ROOT, 'promptTypes'],
    queryFn: ({ signal }) => getServicePromptTypes(signal),
    enabled,
    retry: false,
  });

  const prompts = useQuery({
    queryKey: [...AI_EDIT_QUERY_ROOT, 'prompts', projectId],
    queryFn: ({ signal }) => getServicePrompts(projectId ?? '', signal),
    enabled,
    retry: false,
  });

  const authored = findServicePrompt(prompts.data, AI_EDIT_INSTRUCTIONS_PROMPT_KEY);
  const fallback = servicePromptDefaultsByKey(promptTypes.data)[AI_EDIT_INSTRUCTIONS_PROMPT_KEY] ?? '';
  const basePrompt = authored.trim() !== '' ? authored : fallback;

  return {
    // Not while the default is still loading: a click then would run on the
    // version's stale model, which is the defect this read exists to remove.
    isAvailable: enabled && !model.isLoading && basePrompt.trim() !== '',
    basePrompt,
    modelName,
    isResolving: model.isLoading || (enabled && (promptTypes.isLoading || prompts.isLoading)),
  };
}
