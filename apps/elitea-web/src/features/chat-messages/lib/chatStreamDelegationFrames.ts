/**
 * lib/chatStreamDelegationFrames.ts: `delegation_refused` and
 * `delegation_narrowed` (client contract 1.60, ADR-0044 Amendment 1, A1.2
 * rule 5).
 *
 * A run that refused or narrowed a delegation to a saved agent or pipeline
 * called as a tool streams the notice beside the callee's tool-call frames.
 * It folds into the answer's `delegationNotices` with the shared rules of
 * `@elitea/client-core/stream` (one per call, bounded), the same as the
 * reload's read of `meta.delegation_notices_v1`, so the live notice and the
 * reloaded notice agree. The frame never touches the answer's text or tool
 * actions.
 */
import { delegationNoticeOf, foldDelegationNotice } from '@elitea/client-core/stream';

import { createAssistantMessage, replaceAt, type ChatStreamContext } from './chatStreamShared';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

export function reduceDelegationFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  context: ChatStreamContext,
  index: number,
): readonly ChatMessage[] | undefined {
  if (type !== SocketMessageType.DelegationRefused && type !== SocketMessageType.DelegationNarrowed) return undefined;
  const notice = delegationNoticeOf(frame.response_metadata);
  if (notice === undefined) return history;
  if ((type === SocketMessageType.DelegationRefused) !== (notice.kind === 'refused')) return history;
  const current = history[index];
  if (!current) {
    // FU-360: only assembly-time `pipeline_node` `delegation_refused` frames
    // arrive before `agent_start` (and before `execution.failed`). Create the
    // in-flight assistant message so the notice appears live and survives
    // `settleInFlight` on `execution.failed`.
    if (type !== SocketMessageType.DelegationRefused || notice.edgeKind !== 'pipeline_node') return history;
    return [...history, { ...createAssistantMessage(frame, context), delegationNotices: foldDelegationNotice([], notice) }];
  }
  return replaceAt(history, index, { delegationNotices: foldDelegationNotice(current.delegationNotices ?? [], notice) });
}
