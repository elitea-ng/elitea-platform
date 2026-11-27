/**
 * What an agent's model chip offers (issue 1228, IPC.md "Model per turn"; one chip per agent since #3145, IPC.md
 * "Model per agent" and `agentModels.ts`).
 *
 * - The models: the project's chat models (`listConfigurationModels`, `section=llm`, its own and the shared ones), the
 *   read the host checks a choice against. `/model <name>` takes only one of them (any case, the catalogue's spelling
 *   kept), and none before they are loaded.
 * - The agent's own model: its version's `llm_settings` (an unpinned participant, version 0, has none shown).
 * - Offered: not when the agent's version pins its model (`llm_settings.pin_model`) nor when the workspace's settings
 *   say `allow_model_override: false`; never for a cloud agent (application id 0 here).
 *
 * The host checks every choice before the turn starts and the gateway governs it: nothing here decides what a person
 * may use.
 */
import { useMemo, type ComponentProps } from 'react';

import { useQuery } from '@tanstack/react-query';

import { composer } from '@/features/workspace';
import { useGetApplicationVersionDetail } from '@/shared/api/generated/applications/applications';
import { useListConfigurationModels } from '@/shared/api/generated/configurations/configurations';
import { unwrapBody } from '@/shared/api/unwrap';
import { modelOverrideAllowed, toReasoningEffort, type ReasoningEffort } from '@/shared/desktop/turnModel';
import { t } from '@/shared/i18n';

/** One chat model the chip offers. */
type ModelOption = ComponentProps<typeof composer.ModelChip>['models'][number];

interface AgentModel {
  name: string | null;
  effort: ReasoningEffort | undefined;
  pinned: boolean;
}

interface CatalogueRow {
  name?: unknown;
  type?: unknown;
  description?: unknown;
  supports_reasoning?: unknown;
}

/** The catalogue's chat models, by name, the first of each name kept (the project's own come first). */
export function chatModels(body: unknown): ModelOption[] {
  const items = (unwrapBody(body) as { items?: unknown } | undefined)?.items;
  const seen = new Set<string>();
  const out: ModelOption[] = [];
  for (const row of Array.isArray(items) ? (items as CatalogueRow[]) : []) {
    if (typeof row.name !== 'string' || row.name === '' || seen.has(row.name)) continue;
    if (row.type !== undefined && row.type !== 'llm_model') continue;
    seen.add(row.name);
    out.push({ name: row.name, description: typeof row.description === 'string' && row.description !== '' ? row.description : undefined, supportsReasoning: row.supports_reasoning === true });
  }
  return out;
}

/** The agent version's own model, effort and pin (`llm_settings`). */
export function agentModel(body: unknown): AgentModel {
  const settings = (unwrapBody(body) as { llm_settings?: Record<string, unknown> } | undefined)?.llm_settings ?? {};
  const name = settings['model_name'];
  const effort = settings['reasoning_effort'];
  return { name: typeof name === 'string' && name !== '' ? name : null, effort: toReasoningEffort(effort), pinned: settings['pin_model'] === true };
}

/** `/model <name>`: the listed model of that name (any case; the catalogue's spelling), else why none. */
export function resolveModelName(models: readonly ModelOption[] | undefined, name: string): { name: string } | { notice: string } {
  if (models === undefined) return { notice: t('workspace.model.loading', 'The models are still loading. Try again in a moment.') };
  const wanted = name.trim().toLowerCase();
  const listed = models.find((model) => model.name.toLowerCase() === wanted);
  return listed === undefined ? { notice: t('workspace.model.unknown', 'This project has no chat model named “{{name}}”.', { name: name.trim() }) } : { name: listed.name };
}

export interface AgentModelOptions {
  /** A choice is offered for this agent. */
  offered: boolean;
  /** The agent version's own model; `null` while unknown (or an unpinned participant's). */
  own: string | null;
  ownEffort: ReasoningEffort | undefined;
  pinned: boolean;
  models: ModelOption[];
  /** The catalogue, once loaded (`/model <name>` checks a name against it). */
  loaded: readonly ModelOption[] | undefined;
}

/** What one agent's chip offers; `applicationId` 0: none (a cloud agent, or no agent yet). */
export function useAgentModelOptions(workspaceId: string, projectId: number, applicationId: number, versionId: number): AgentModelOptions {
  const enabled = applicationId > 0;
  const version = useGetApplicationVersionDetail(String(projectId), applicationId, versionId, { query: { enabled: enabled && versionId > 0, staleTime: 30_000 } });
  const allowed = useQuery({ queryKey: ['workspace', 'model-override', workspaceId], queryFn: () => modelOverrideAllowed(workspaceId), staleTime: 30_000, enabled });
  const own = useMemo(() => agentModel(version.data), [version.data]);
  // A pinned version offers no choice: with a version to read, nothing is offered until it was read.
  const offered = enabled && (versionId === 0 || version.isSuccess) && allowed.data !== false && !own.pinned;
  const catalogue = useListConfigurationModels(String(projectId), { section: 'llm', include_shared: true }, { query: { enabled: offered, staleTime: 60_000 } });
  const models = useMemo(() => chatModels(catalogue.data), [catalogue.data]);
  return { offered, own: own.name, ownEffort: own.effort, pinned: own.pinned, models, loaded: catalogue.isSuccess ? models : undefined };
}
