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
  readonly llm?: { readonly settings?: Readonly<Record<string, unknown>>; readonly onSetSettings?: (settings: Readonly<Record<string, unknown>>) => void } | undefined;
  readonly setSelectedModel: (model: { readonly name?: string; readonly projectId?: string; readonly supportsReasoning?: boolean } | null) => void;
}

export interface UseChatBoxModelSelectionResult {
  readonly modelsList: readonly LLMModel[];
  readonly selectedLlmModel: LLMModel | null;
  readonly handleSelectModel: (model: LLMModel) => void;
}

/** Accept primitive project identities only; absent settings retain ordinary chat selection. */
function matchesModelProject(projectId: string, configuredProject: unknown): boolean {
  if (configuredProject == null) return true;
  if (typeof configuredProject !== 'string' && typeof configuredProject !== 'number') return false;
  return projectId === String(configuredProject);
}

export function useChatBoxModelSelection({
  projectId,
  selectedModelName,
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
    const name = typeof configuredName === 'string' && configuredName ? configuredName : selectedModelName;
    const raw = modelsData?.items.find((m) => m.name === name &&
      matchesModelProject(m.project_id, configuredProject));
    return raw ? toLlmModel(raw) : null;
  }, [modelsData?.items, selectedModelName, configuredName, configuredProject]);
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
