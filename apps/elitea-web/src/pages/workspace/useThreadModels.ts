/**
 * Each agent's model, as the thread composer offers it (#3145, `agentModels`): the agents whose model the thread may
 * choose (Thread settings › Models), `/model`'s agent (the lead, else the composer's agent) and `/model <name>`.
 */
import { useState } from 'react';

import { floorOf } from '@elitea/client-core/conversation';
import { useSidePanelSlot } from '@/shared/desktop/inspectorSlot';

import type { AgentModels } from './agentModels';
import { participantsTabId, pendingAgents } from './ThreadAgentsPanel';
import type { ModelAgent } from './ThreadSettings';
import type { AgentSelection } from './useAgentSelection';
import type { AgentModelEntry, ThreadFloor } from './useSendPrompt';
import { resolveModelName, useAgentModelOptions } from './useTurnModel';

/**
 * The agents whose model the thread may choose (#3145): the joined ones (an unpinned participant runs the composer's
 * version when it is the composer's agent), then the ones that join with the next message.
 */
export function modelAgents(selection: AgentSelection, floor: ThreadFloor | undefined): ModelAgent[] {
  const joined = floor?.agents ?? [];
  const versionOf = (applicationId: number, versionId: number): number => (versionId === 0 && String(applicationId) === selection.agentId ? Number(selection.versionId) || 0 : versionId);
  const rows: ModelAgent[] = joined.map((agent) => ({ applicationId: agent.applicationId, versionId: versionOf(agent.applicationId, agent.versionId), name: agent.name, cloud: agent.cloud }));
  for (const pending of pendingAgents(selection, joined)) {
    const companion = selection.companions?.find((c) => c.agentId === pending.agentId && c.cloud === pending.cloud);
    const versionId = pending.agentId === selection.agentId ? Number(selection.versionId) || 0 : (companion?.versionId ?? 0);
    rows.push({ applicationId: Number(pending.agentId), versionId, name: pending.name, cloud: pending.cloud });
  }
  // One row per agent: a participant listed twice (or joined and pending) is one model choice.
  const seen = new Set<string>();
  return rows.filter((row) => {
    const key = `${row.applicationId}:${row.cloud}`;
    if (row.applicationId <= 0 || seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

/** `/model`'s agent: the lead, else the composer's agent; a thread-wide choice made before #3145 is the lead's too. */
export function defaultModelAgent(agents: readonly ModelAgent[], floor: ThreadFloor | undefined, selection: AgentSelection): ModelAgent | undefined {
  const lead = floor === undefined ? 0 : floorOf(floor.meta).leadParticipantId;
  const leadApp = floor?.agents.find((agent) => agent.participantId === lead)?.applicationId;
  return agents.find((agent) => agent.applicationId === leadApp) ?? agents.find((agent) => String(agent.applicationId) === selection.agentId) ?? agents[0];
}

/** `/model <name>` and `/model`, for the default agent; the choices made here go when the host refuses a chosen model. */
/** The refusals of a chosen model: the agent pins its own, the folder turned choices off, or the project lost it. */
const DROPS_CHOICES: ReadonlySet<string> = new Set(['model_unavailable', 'model_pinned', 'model_override_disabled']);

/** A new refusal of a chosen model drops the choices made here, so the next send does not carry them again. */
function useDropOnRefusal(models: AgentModels, refusalCode: string | null): void {
  const [seen, setSeen] = useState(refusalCode);
  if (refusalCode === seen) return;
  setSeen(refusalCode);
  if (refusalCode !== null && DROPS_CHOICES.has(refusalCode)) models.dropPending();
}

interface ModelCommandsInput {
  workspaceId: string;
  projectId: number;
  models: AgentModels;
  /** The thread's agents (Thread settings › Models): `/model` shows the tab they make. */
  agents: readonly ModelAgent[];
  /** `/model`'s agent. */
  target: ModelAgent | undefined;
  local: boolean;
  /** The turn's refused start's code: a new model refusal drops the choices made here. */
  refusalCode: string | null;
}

export function useModelCommands({ workspaceId, projectId, models, agents, target, local, refusalCode }: ModelCommandsInput) {
  const panel = useSidePanelSlot();
  const open = (): void => panel?.show(participantsTabId(agents.length));
  const agentForModel = local && target !== undefined && !target.cloud ? target : undefined;
  const options = useAgentModelOptions(workspaceId, projectId, agentForModel?.applicationId ?? 0, agentForModel?.versionId ?? 0);
  const [notice, setNotice] = useState<string | null>(null);
  useDropOnRefusal(models, refusalCode);
  const offered = agentForModel !== undefined && options.offered;
  const command = (name: string): void => {
    if (agentForModel === undefined) return;
    const resolved = resolveModelName(options.loaded, name);
    if ('notice' in resolved) setNotice(resolved.notice);
    else {
      setNotice(null);
      models.choose(agentForModel.applicationId, { name: resolved.name }, options.own);
    }
  };
  const openChip = (): boolean => {
    if (!offered || agentForModel === undefined) return false;
    open();
    models.edit(agentForModel.applicationId);
    return true;
  };
  // A local thread's send carries the choices made here; the cloud copy's own agent answers with its own model.
  const entries = (): AgentModelEntry[] | undefined => (local ? models.entries() : undefined);
  return { onModelCommand: offered ? command : undefined, openChip, notice, dismissNotice: () => setNotice(null), entries };
}
