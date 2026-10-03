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
  readonly selectedModelName: string | undefined;
  /** The project of the saved-or-default model; it identifies the implied default when no model is configured. */
  readonly selectedModelProjectId?: string | number | undefined;
  readonly llm?: { readonly settings?: Readonly<Record<string, unknown>>; readonly onSetSettings?: (settings: Readonly<Record<string, unknown>>) => void } | undefined;
  readonly setSelectedModel: (model: { readonly name?: string; readonly projectId?: string; readonly supportsReasoning?: boolean } | null) => void;
}

export interface UseChatBoxModelSelectionResult {
  readonly modelsList: readonly LLMModel[];
  readonly selectedLlmModel: LLMModel | null;
  readonly handleSelectModel: (model: LLMModel) => void;
}

/** Accept primitive project identities only; absent settings retain ordinary chat selection. */
function matchesModelProject(projectId: string | number, configuredProject: unknown): boolean {
  if (configuredProject == null) return true;
  if (typeof configuredProject !== 'string' && typeof configuredProject !== 'number') return false;
  return String(projectId) === String(configuredProject);
}

export function useChatBoxModelSelection({
  projectId,
  selectedModelName,
  selectedModelProjectId,
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
  const selectedLlmModel = useMemo(() => {
    const hasConfiguredName = typeof configuredName === 'string' && configuredName !== '';
    const name = hasConfiguredName ? configuredName : selectedModelName;
    // The implied default (no configured model name) lives in ITS OWN project,
    // which is often the shared project 1, not the chat's project (UI-PD-2).
    // Match it on the default model's project; a stray configured project id
    // must not hide it.
    const project = hasConfiguredName ? configuredProject : selectedModelProjectId;
    const raw = modelsData?.items.find((m) => m.name === name &&
      matchesModelProject(m.project_id, project));
    return raw ? toLlmModel(raw) : null;
  }, [modelsData?.items, selectedModelName, selectedModelProjectId, configuredName, configuredProject]);
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

  return { modelsList, selectedLlmModel, handleSelectModel };
}
