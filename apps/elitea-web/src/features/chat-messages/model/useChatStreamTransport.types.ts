import type { EditorTestRun } from '@/shared/api/generated/model';
import type { ExecutionEventData } from '@/shared/api/sse';
import type { ChatMessage } from '../lib/convertMessagesToChatHistory';
import type { ChatStreamContext } from '../lib/chatStreamReducer';
import type { ChatStreamRunStarters } from './useChatStreamRunStarters';

type SetChatHistory = (
  updater: (prev: readonly ChatMessage[]) => readonly ChatMessage[],
) => void;

/** @public Params for `useChatStreamTransport`. */
export interface UseChatStreamTransportParams {
  readonly setChatHistory: SetChatHistory;
  /**
   * The conversation currently on screen. A stream whose owner (the
   * `conversationUuid` its `start` was called with) stops matching this is
   * closed, and its frames are dropped rather than folded into the
   * conversation that is now mounted — issue #328. Leaving it undefined
   * disables the guard, which is only right for a caller that has exactly one
   * conversation for its whole lifetime.
   */
  readonly conversationUuid?: string | undefined;
  /** Identity for messages the reducer has to create, plus the participant roster. */
  readonly context?: ChatStreamContext | undefined;
  /**
   * Graph frames, for a surface that renders a run timeline (the pipeline
   * flow editor). Chat itself ignores them; forwarding is the caller's half of
   * the baseline's `onRcvAgentEvent`, see `agentGraphEvents.ts`.
   */
  readonly onAgentEvent?: ((frame: ExecutionEventData) => void) | undefined;
  /** Accepted progress/lifecycle changes invalidate the durable context read model. */
  readonly onContextChanged?: ((projectId: string | number) => void) | undefined;
  /** The run itself failed server-side, or its stream dropped. */
  readonly onStreamError?: ((reason: string) => void) | undefined;
}

/**
 * @public
 *
 * `startDetailed`, `start`, `resume` and `regenerate` come from
 * `ChatStreamRunStarters`, which documents each one — including the boolean
 * every caller reads to decide whether a socket fallback is still safe.
 */
export interface UseChatStreamTransportResult extends ChatStreamRunStarters {
  /** Whether a stream is currently subscribed — drives the composer's Stop affordance. */
  readonly isStreaming: boolean;
  /**
   * Detach from the current stream WITHOUT cancelling the run: close the
   * connection, cancel any pending reconnect, and leave the history alone.
   *
   * This is the conversation-switch / teardown path. It does not settle the
   * spinner because the history it would settle is the one now on screen,
   * which belongs to a different conversation.
   */
  readonly attachExistingRun: (target: { readonly projectId: string | number; readonly conversationUuid: string; readonly run: EditorTestRun }) => boolean;
  readonly close: () => void;
  /**
   * The user pressed Stop. Cancels the run SERVER-SIDE (`DELETE
   * /elitea_core/task/prompt_lib/{projectId}/{responseMessageId}` — the id the
   * start endpoint returned). Observe the terminal event before settling
   * the message. Keep reconnecting while cancellation is pending.
   */
  readonly stop: () => void;
}
