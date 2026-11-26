/** A refused first send of a new thread: retried in the thread it created, or left with a new client id (#3077). */

/**
 * A conversation created for a send that did not start (the participants add
 * failed, or the host refused the start), and the agent version it was
 * created for. `ready`: its participants were added.
 */
export interface PendingConversation {
  key: string;
  id: string;
  ready: boolean;
}

/**
 * The pending conversation a "new conversation" send for `key` continues in (`reuse`), if any, and whether a pending
 * one is left for another agent or version (`abandoned`): that send needs a new client id (#3077), or Main would answer
 * the left conversation, with the other agent in it, again.
 */
export function retryTarget(pending: PendingConversation | null, startsNew: boolean, key: string): { reuse: PendingConversation | null; abandoned: boolean } {
  const reuse = startsNew && pending?.key === key ? pending : null;
  return { reuse, abandoned: startsNew && pending !== null && reuse === null };
}

/** The send's `retryTarget`, forgetting an abandoned pending conversation and its client id. */
export function takeRetry(pending: { current: PendingConversation | null }, submitIds: { sent: () => void }, startsNew: boolean, key: string): PendingConversation | null {
  const { reuse, abandoned } = retryTarget(pending.current, startsNew, key);
  if (abandoned) {
    pending.current = null;
    submitIds.sent();
  }
  return reuse;
}
