/**
 * lib/delegationNoticesOf.ts: the reload's read of `meta.delegation_notices_v1`
 * (client contract 1.60, ADR-0044 Amendment 1): the same notices the live
 * `delegation_refused` and `delegation_narrowed` frames fold
 * (`chatStreamDelegationFrames.ts`).
 */
import { parseDelegationNotices } from '@elitea/client-core/stream';

import type { ChatMessage } from './convertMessagesToChatHistory.types';

export function delegationNoticesOf(raw: unknown): Pick<ChatMessage, 'delegationNotices'> {
  const notices = parseDelegationNotices(raw);
  return notices.length === 0 ? {} : { delegationNotices: notices };
}
