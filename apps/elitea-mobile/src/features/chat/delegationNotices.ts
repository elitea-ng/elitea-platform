import { missingNames, parseDelegationNotices, type DelegationNotice } from '@elitea/client-core/stream';

/**
 * A refused or narrowed delegation of a cloud answer (client contract 1.60,
 * ADR-0044 Amendment 1) as the sentences the thread shows. Live turns fold the
 * `delegation_refused` / `delegation_narrowed` frames (`foldDelegationNotice`);
 * a reloaded answer reads `meta.delegation_notices_v1`. Every text is plain
 * text: the names are untrusted.
 */

/**
 * The approve screen of a calling agent's delegation. One place, so the route is a one-line change.
 * `callee` (the called agent's id) puts that edge first and marks it, so the person lands on the
 * grant the notice is about; `name` titles the screen with the caller.
 */
export const delegationReviewHref = (callerId: number, calleeId?: number, callerName?: string): string => {
  const query = [calleeId === undefined || !Number.isSafeInteger(calleeId) || calleeId < 1 ? '' : `callee=${calleeId}`, callerName === undefined ? '' : `name=${encodeURIComponent(callerName)}`].filter((part) => part !== '');
  return query.length === 0 ? `/agent-delegations/${callerId}` : `/agent-delegations/${callerId}?${query.join('&')}`;
};

/** A notice's stored list; a streaming answer and a settled one read it the same. */
export function storedDelegationNotices(meta: unknown): DelegationNotice[] {
  const raw = meta !== null && typeof meta === 'object' && !Array.isArray(meta) ? (meta as Record<string, unknown>).delegation_notices_v1 : undefined;
  return [...parseDelegationNotices(raw)];
}

/** "Git, Jira and 2 more": the names the notice lists, and how many more there were. */
function namesOf(notice: DelegationNotice): string {
  const names = missingNames(notice);
  const more = notice.missingTotal - names.length;
  if (names.length === 0) return 'a toolkit';
  return more > 0 ? `${names.join(', ')} and ${more} more` : names.join(', ');
}

/** The sentence of one notice. */
export function delegationNoticeText(notice: DelegationNotice): string {
  const callee = notice.calleeName;
  switch (notice.reason) {
    case 'depth':
      return `${callee} was not called: the delegation depth limit${notice.maxDepth === undefined ? '' : ` (${notice.maxDepth})`} was reached.`;
    case 'principal':
      return `${callee} could not use ${namesOf(notice)}. You do not have access to it.`;
    case 'stale':
      return `${callee} ran without ${namesOf(notice)}. It was added after this delegation was approved.`;
    case 'no_grant':
      return `${callee} could not use ${namesOf(notice)} from ${notice.callerName ?? 'its caller'}. This delegation is not approved.`;
  }
}

/** For a viewer who may not review: who to ask, or null when the server named nobody. */
export function delegationAskText(notice: DelegationNotice): string | null {
  return notice.editors.length === 0 ? null : `Ask ${notice.editors.join(', ')} to review this delegation.`;
}
