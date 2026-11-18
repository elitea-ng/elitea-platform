/**
 * The agent page's test chat shows a model picker in its composer, and the
 * agent's own form has one too (`AgentModelSettings`). They are ONE setting:
 * the model the agent version runs on (`version_details.llm_settings`). This
 * binds the chat's picker to the form's value, so a model picked in the form
 * shows in the chat at once, and a model picked in the chat lands in the form
 * (and so in Save and the unsaved-changes guard) instead of in a private
 * `useState` of the chat that no run ever read (#2151).
 *
 * The result is `ChatBox`'s `llm` prop: `settings` is what the picker reads,
 * `onSetSettings` is what it calls with `{model_name, model_project_id}` on a
 * model pick, or with tunables alone from its settings dialog.
 */
import { useCallback, useMemo } from 'react';

import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { useListModelsQuery, type ConfigModel } from '@/shared/api/configurationsApi';

import { writeAgentLlmSettings, type AgentLlmTunables } from './writeAgentLlmSettings';

export interface AgentChatModelBinding {
  /** `undefined` while the version pins no model: the chat then shows the project default, as the form does. */
  readonly settings: Readonly<Record<string, unknown>> | undefined;
  readonly onSetSettings: (settings: Readonly<Record<string, unknown>>) => void;
}

function projectOf(value: unknown): number | undefined {
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

/** The tunables of a settings bag, read by type: the dialog hands back a loose record. */
function tunablesOf(bag: Readonly<Record<string, unknown>>): AgentLlmTunables {
  const { max_tokens: maxTokens, temperature, reasoning_effort: effort } = bag;
  return {
    ...(typeof maxTokens === 'number' || typeof maxTokens === 'string' ? { max_tokens: maxTokens } : {}),
    ...(typeof temperature === 'number' ? { temperature } : {}),
    ...(typeof effort === 'string' ? { reasoning_effort: effort } : {}),
  };
}

/** The row a name (and project, when given) names; the project default when no name is given. */
function rowFor(
  items: readonly ConfigModel[],
  name: unknown,
  project: number | undefined,
  defaultName: string | undefined,
): ConfigModel | undefined {
  if (typeof name === 'string' && name !== '') {
    return items.find((item) => item.name === name && (project === undefined || projectOf(item.project_id) === project));
  }
  return (
    items.find((item) => item.name === defaultName && item.default === true) ??
    items.find((item) => item.default === true) ??
    items[0]
  );
}

export function useAgentChatModelBinding(
  projectId: string | undefined,
  value: AgentLlmSettings | undefined,
  onChange: (next: AgentLlmSettings) => void,
  disabled: boolean,
): AgentChatModelBinding | undefined {
  const { data } = useListModelsQuery(
    { projectId: projectId ?? '', include_shared: true },
    { enabled: projectId !== undefined },
  );
  const items = useMemo(() => data?.items ?? [], [data?.items]);
  const defaultName = data?.default_model_name;

  const onSetSettings = useCallback(
    (next: Readonly<Record<string, unknown>>) => {
      const name = next['model_name'] ?? value?.model_name;
      const project = projectOf(next['model_project_id'] ?? (next['model_name'] === undefined ? value?.model_project_id : undefined));
      const row = rowFor(items, name, project, defaultName);
      if (row === undefined) return;
      // A model pick brings no tunables of its own; the version's own carry over, as in the form's picker.
      const written = writeAgentLlmSettings(
        { name: row.name, projectId: row.project_id, supportsReasoning: row['supports_reasoning'] === true },
        next['model_name'] === undefined ? tunablesOf({ ...value, ...next }) : value,
      );
      if (written !== undefined) onChange(written);
    },
    [items, defaultName, value, onChange],
  );

  return useMemo(
    () => (disabled ? undefined : { settings: value === undefined ? undefined : { ...value }, onSetSettings }),
    [disabled, value, onSetSettings],
  );
}
