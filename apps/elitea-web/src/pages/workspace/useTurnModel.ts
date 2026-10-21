/**
 * What the composer's model chip shows and what the next send asks for
 * (issue 1228, IPC.md "Model per turn").
 *
 * - The models: the project's chat models (`listConfigurationModels`,
 *   `section=llm`, its own and the shared ones), the read the host checks a
 *   choice against.
 * - The current model: the one chosen here, else the one the thread's
 *   latest turn ran on (its `status` `running` `model`, live or recorded;
 *   the host keeps a thread's choice across restarts), else the agent
 *   version's own (`llm_settings`).
 * - Offered: not when the agent pins its model (`llm_settings.pin_model`,
 *   or a turn said `pinned`) nor when the workspace's settings say
 *   `allow_model_override: false`.
 *
 * A choice made here goes with every send of this thread from then on
 * (`agent_turn_start` `model`); without one the host runs the thread's kept
 * model.
 */
import { useMemo, useState } from 'react';

import { useQuery } from '@tanstack/react-query';

import type { ModelOption, WorkspaceTurn } from '@/features/workspace';
import { useGetApplicationVersionDetail } from '@/shared/api/generated/applications/applications';
import { useListConfigurationModels } from '@/shared/api/generated/configurations/configurations';
import { unwrapBody } from '@/shared/api/unwrap';
import { isReasoningEffort, modelOverrideAllowed, type ReasoningEffort, type TurnModel, type TurnModelChoice } from '@/shared/desktop/turnModel';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import type { AgentSelection } from './useAgentSelection';
import type { ThreadHistory } from './useThreadHistory';

export interface TurnModelControl {
  /** The chip is shown. */
  offered: boolean;
  current: string | null;
  effort: ReasoningEffort | undefined;
  models: ModelOption[];
  /** What the next send carries; `undefined`: the thread's kept model. */
  choice: TurnModelChoice | undefined;
  choose: (choice: TurnModelChoice) => void;
}

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

type StatusEvent = Extract<AgentEvent, { kind: 'status' }>;

function statusModel(events: readonly AgentEvent[]): TurnModel | undefined {
  return events.findLast((event): event is StatusEvent => event.kind === 'status' && event.payload.model !== undefined)?.payload.model;
}

/** The model the thread's latest turn ran on: a turn of this session, else a recorded one. */
export function latestTurnModel(turn: Pick<WorkspaceTurn, 'view' | 'earlier'>, history: ThreadHistory): TurnModel | undefined {
  const live = turn.view.model ?? turn.earlier.findLast((entry) => entry.view.model !== undefined)?.view.model;
  if (live !== undefined || history.kind !== 'local') return live;
  return history.turns.map((stored) => statusModel(stored.events)).findLast((model) => model !== undefined);
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
  return { name: typeof name === 'string' && name !== '' ? name : null, effort: isReasoningEffort(effort) ? effort : undefined, pinned: settings['pin_model'] === true };
}

const positiveId = (value: string): number => {
  const id = Number(value);
  return Number.isInteger(id) && id > 0 ? id : 0;
};

/** The chip's model: the choice made here, else the latest turn's, else the agent's own. */
function shownModel(choice: TurnModelChoice | undefined, latest: TurnModel | undefined, own: AgentModel): Pick<TurnModelControl, 'current' | 'effort'> {
  if (choice !== undefined) return { current: choice.name, effort: choice.reasoning_effort };
  if (latest !== undefined) return { current: latest.name, effort: latest.reasoning_effort };
  return { current: own.name, effort: own.effort };
}

export function useTurnModel(workspaceId: string, projectId: number, selection: AgentSelection, turn: WorkspaceTurn, history: ThreadHistory): TurnModelControl {
  const [choice, setChoice] = useState<TurnModelChoice | undefined>(undefined);
  const agentId = positiveId(selection.agentId);
  const versionId = positiveId(selection.versionId);
  const version = useGetApplicationVersionDetail(String(projectId), agentId, versionId, { query: { enabled: agentId > 0 && versionId > 0, staleTime: 30_000 } });
  const allowed = useQuery({ queryKey: ['workspace', 'model-override', workspaceId], queryFn: () => modelOverrideAllowed(workspaceId), staleTime: 30_000 });
  const own = useMemo(() => agentModel(version.data), [version.data]);
  const latest = latestTurnModel(turn, history);
  const offered = allowed.data !== false && !own.pinned && latest?.pinned !== true;
  const catalogue = useListConfigurationModels(String(projectId), { section: 'llm', include_shared: true }, { query: { enabled: offered, staleTime: 60_000 } });
  const models = useMemo(() => chatModels(catalogue.data), [catalogue.data]);
  return {
    offered,
    ...shownModel(choice, latest, own),
    models,
    choice: offered ? choice : undefined,
    choose: setChoice,
  };
}
