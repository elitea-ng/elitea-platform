import type { ConversationAction } from '@/api/queries/conversationActions';
import { log } from '@/lib/log';

/**
 * Who tells the user that the workspace refused a queued Chats-list action: exactly one owner.
 *
 * Screens stack (Archived chats is pushed over the Chats tab, which stays mounted), and each
 * listens. A failure is offered to the newest listener first; the first one that answers
 * `true` (it told the user) ends it, so a refused unarchive is said once, by Archived chats
 * while it is open, and by the Chats tab once it is closed.
 */

export interface FailedOp {
  projectId: number;
  conversationId: number;
  action: ConversationAction;
}

/** Returns true when it took the failure (told the user); false or nothing passes it on. */
export type ActionFailureListener = (op: FailedOp) => boolean | void;

const listeners: ActionFailureListener[] = [];

/** Told when the workspace refused an action (the list goes back to what it says). */
export function subscribeActionFailures(listener: ActionFailureListener): () => void {
  listeners.push(listener);
  return () => {
    const at = listeners.lastIndexOf(listener);
    if (at !== -1) listeners.splice(at, 1);
  };
}

/** Offers the failure to the newest listener first, until one takes it. */
export function reportActionFailure(op: FailedOp): void {
  for (const listener of [...listeners].reverse()) {
    try {
      if (listener(op) === true) return;
    } catch (error) {
      log.warn('action failure listener failed', { error });
    }
  }
}
