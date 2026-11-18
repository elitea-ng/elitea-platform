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

import { replaceAt } from './chatStreamShared';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

export function reduceDelegationFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  index: number,
): readonly ChatMessage[] | undefined {
  if (type !== SocketMessageType.DelegationRefused && type !== SocketMessageType.DelegationNarrowed) return undefined;
  const current = history[index];
  const notice = delegationNoticeOf(frame.response_metadata);
  if (!current || notice === undefined) return history;
  return replaceAt(history, index, { delegationNotices: foldDelegationNotice(current.delegationNotices ?? [], notice) });
}
