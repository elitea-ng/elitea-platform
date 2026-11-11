/**
 * lib/chatStreamSubAgentFrames.ts — `agent_subagent` (client contract 1.48,
 * ADR-0050 decision 4).
 *
 * A cloud run that starts temporary sub-agents streams each child's
 * lifecycle on the answer's own stream. The entries fold into the answer's
 * `subAgents` with the shared rules of `@elitea/client-core/stream`, the same
 * as Main's fold onto `meta.subagents_v1`, so the live lane and the reloaded
 * lane agree. The frame never touches the answer's text or tool actions.
 */
import { foldSubAgentEntry, settleSubAgentLanes, subAgentEntryOf, type CloudSubAgentLane } from '@elitea/client-core/stream';

import { replaceAt } from './chatStreamShared';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

export function reduceSubAgentFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  index: number,
): readonly ChatMessage[] | undefined {
  if (type !== SocketMessageType.AgentSubAgent) return undefined;
  const current = history[index];
  if (!current) return history;
  const lanes = current.subAgents ?? [];
  const next = foldSubAgentEntry(lanes, subAgentEntryOf(frame));
  return next === lanes ? history : replaceAt(history, index, { subAgents: next });
}

/** The turn ended: a lane the worker never ended was stopped with it. */
export function settleSubAgents(lanes: readonly CloudSubAgentLane[] | undefined): readonly CloudSubAgentLane[] | undefined {
  return lanes === undefined ? undefined : settleSubAgentLanes(lanes);
}
