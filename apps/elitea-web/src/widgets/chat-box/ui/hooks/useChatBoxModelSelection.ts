/**
 * Split out of `ChatBox.tsx` to stay under the file-length/component-props
 * budgets (§3.5) — the LLM model list + selection wiring for the chat
 * input's `LLMModelSelector` slot.
 */
import { useCallback, useMemo } from 'react';

import { useListModelsQuery } from '@/shared/api/configurationsApi';
import { toLlmModel, type LLMModel } from '@/widgets/llm-model-selector';

export interface UseChatBoxModelSelectionParams {
  readonly projectId: string | number | undefined;
  /** The saved-or-default model. Its project identifies the implied default when no model is configured (UI-PD-2). */
  readonly selectedModel: { readonly name?: string | undefined; readonly projectId?: string | number | undefined } | null | undefined;
  readonly llm?: { readonly settings?: Readonly<Record<string, unknown>>; readonly onSetSettings?: (settings: Readonly<Record<string, unknown>>) => void } | undefined;
  readonly setSelectedModel: (model: { readonly name?: string; readonly projectId?: string; readonly supportsReasoning?: boolean } | null) => void;
}

export interface UseChatBoxModelSelectionResult {
  readonly modelsList: readonly LLMModel[];
  readonly selectedLlmModel: LLMModel | null;
  readonly handleSelectModel: (model: LLMModel) => void;
  /**
   * The project of the model the picker resolved for a configured
   * `model_name` that names no `model_project_id` (legacy rows, other
   * clients). The send path adds it, so the turn runs the model the picker
   * shows. Undefined in every other case.
   */
  readonly configuredModelProjectId: number | undefined;
}

/** Accept primitive project identities only; absent settings retain ordinary chat selection. */
function matchesModelProject(projectId: string | number, configuredProject: unknown): boolean {
  if (configuredProject == null) return true;
  if (typeof configuredProject !== 'string' && typeof configuredProject !== 'number') return false;
  return String(projectId) === String(configuredProject);
}

interface ModelLike {
  readonly name: string;
  readonly project_id: string | number;
}

interface ModelSelectionKeys {
  readonly configuredName: unknown;
  readonly configuredProject: unknown;
  readonly chatProjectId: string | number | undefined;
  readonly selectedModelName: string | undefined;
  readonly selectedModelProjectId: string | number | undefined;
}

/**
 * The model the picker shows.
 *
 * A configured `model_name` with no `model_project_id` names no project. The
 * server resolves such a name in the chat's own project, so prefer that
 * project's model, and fall back to a same-named shared model only when the
 * chat's project has none. Matching the first same-named row of the
 * include_shared list showed a shared copy while the turn ran the project's.
 */
function findSelectedModel<T extends ModelLike>(items: readonly T[], keys: ModelSelectionKeys): T | undefined {
  const { configuredName, configuredProject, chatProjectId } = keys;
  const hasConfiguredName = typeof configuredName === 'string' && configuredName !== '';
  if (!hasConfiguredName) {
    // The implied default (no configured model name) lives in ITS OWN project,
    // which is often the shared project 1, not the chat's project (UI-PD-2).
    // Match it on the default model's project; a stray configured project id
    // must not hide it.
    return items.find((m) => m.name === keys.selectedModelName && matchesModelProject(m.project_id, keys.selectedModelProjectId));
  }
  if (configuredProject != null) {
    return items.find((m) => m.name === configuredName && matchesModelProject(m.project_id, configuredProject));
  }
  const named = items.filter((m) => m.name === configuredName);
  const own = chatProjectId === undefined ? undefined : named.find((m) => String(m.project_id) === String(chatProjectId));
  return own ?? named[0];
}

function positiveProject(value: string | number | undefined): number | undefined {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
}

/** The resolved model's project, only for a configured name saved with no project id. */
function configuredProjectOf(configuredName: unknown, configuredProject: unknown, resolved: ModelLike | undefined): number | undefined {
  const hasConfiguredName = typeof configuredName === 'string' && configuredName !== '';
  return hasConfiguredName && configuredProject == null ? positiveProject(resolved?.project_id) : undefined;
}

export function useChatBoxModelSelection({
  projectId,
  selectedModel,
  setSelectedModel,
  llm,
}: UseChatBoxModelSelectionParams): UseChatBoxModelSelectionResult {
  const { data: modelsData } = useListModelsQuery(
    { projectId: projectId !== undefined ? String(projectId) : '', include_shared: true },
    { enabled: projectId !== undefined },
  );
  const modelsList = useMemo(() => (modelsData?.items ?? []).map(toLlmModel), [modelsData?.items]);
  const configuredName = llm?.settings?.['model_name'];
  const configuredProject = llm?.settings?.['model_project_id'];
  const selectedModelName = selectedModel?.name;
  const selectedModelProjectId = selectedModel?.projectId;
  const selectedRaw = useMemo(
    () => findSelectedModel(modelsData?.items ?? [], {
      configuredName, configuredProject, chatProjectId: projectId, selectedModelName, selectedModelProjectId,
    }),
    [modelsData?.items, selectedModelName, selectedModelProjectId, configuredName, configuredProject, projectId],
  );
  const selectedLlmModel = useMemo(() => (selectedRaw ? toLlmModel(selectedRaw) : null), [selectedRaw]);
  const configuredModelProjectId = configuredProjectOf(configuredName, configuredProject, selectedRaw);
  const handleSelectModel = useCallback(
    (model: LLMModel) => {
      const raw = modelsData?.items.find((m) => toLlmModel(m).id === model.id && m.name === model.name);
      if (llm?.onSetSettings) {
        if (raw) llm.onSetSettings({ model_name: raw.name, model_project_id: raw.project_id });
        return;
      }
      setSelectedModel(raw ? { name: raw.name, projectId: raw.project_id, supportsReasoning: Boolean(raw['supports_reasoning']) } : { name: model.name });
    },
    [modelsData?.items, setSelectedModel, llm],
  );

  return { modelsList, selectedLlmModel, handleSelectModel, configuredModelProjectId };
}
