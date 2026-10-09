/**
 * The canvas saves still in flight, so a chat turn can wait for them.
 *
 * A canvas's newest version is part of the history the next turn hands the
 * model (agent_chat.sql projects it). Closing the canvas editor PUTs the edit,
 * and the composer is reachable the moment the drawer is gone — so a send
 * typed right after Escape could reach the server before the save did, and
 * the model would answer about the PREVIOUS version of the document the user
 * is looking at. The send path awaits `settleCanvasSaves()` before it starts a
 * turn, the same way it already awaits a pending internal-tools save.
 *
 * Lives in `shared/lib/` because the writer (`processes/chat`) and the reader
 * (`widgets/chat-box`) are in different layers and `shared/` is the one both
 * may import. Module state, not a store: nothing renders from it.
 */

const pending = new Set<Promise<unknown>>();

/** Registers a save; it leaves the set when it settles, whichever way. */
export function trackCanvasSave(save: Promise<unknown>): void {
  pending.add(save);
  const forget = (): void => {
    pending.delete(save);
  };
  save.then(forget, forget);
}

/**
 * Resolves once every save registered so far has settled. It never rejects:
 * a failed save is reported by the editor that made it (which stays open with
 * the user's text), and the send is not the place to surface it again.
 */
export async function settleCanvasSaves(): Promise<void> {
  if (pending.size === 0) return;
  await Promise.allSettled(pending);
}
