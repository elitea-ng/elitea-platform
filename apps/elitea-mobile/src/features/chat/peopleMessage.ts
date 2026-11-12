import { isApiError } from '@/api/errors';
import { TurnStartError, startTurn, type PreparedTurn } from '@/api/queries/sendMessage';
import { log } from '@/lib/log';
import { handOffToOutbox } from '@/sync/appOutbox';

/**
 * Sends a message for people alone (client contract 1.51): it mentions people and addresses no agent.
 * Main stores it as the sender's message and notifies the people; it starts no turn, so there is no
 * live turn, no stream and no pending answer here. The transcript is read again to show the message.
 *
 * Offline, or behind an unsent message, the caller queues it like any other send. Here an unreachable
 * workspace hands the same body (and question_id) to the outbox, which retries it.
 *
 * Resolves to an error text when it could not be sent, `null` when it was stored or queued.
 */
export async function sendPeopleMessage(
  prepared: PreparedTurn,
  deps: { conversationRef: string; onStored: () => void },
): Promise<string | null> {
  try {
    await startTurn(prepared);
    deps.onStored();
    return null;
  } catch (error) {
    if (error instanceof TurnStartError) {
      const failure = error.failure;
      if (failure.kind === 'transient') {
        const queued = await handOffToOutbox(prepared, deps.conversationRef, { retryAfterMs: failure.retryAfterMs });
        return queued ? null : COULD_NOT_SEND;
      }
      if (failure.kind === 'rejected' && failure.message !== null && failure.message !== '') return failure.message;
      log.warn('people-only message refused', { kind: failure.kind });
      return COULD_NOT_SEND;
    }
    if (isApiError(error)) {
      // 401/426/origin: the session layer already reacted.
      return COULD_NOT_SEND;
    }
    log.warn('unexpected people-only send failure', { error });
    return COULD_NOT_SEND;
  }
}

const COULD_NOT_SEND = 'This message could not be sent. Try again.';
