/**
 * `ChatBoxProps`'s bundle unwrapping.
 *
 * Its own file for the same reason `ChatBox.types.ts` and `ChatBox.layout.ts`
 * are: `ChatBox.helpers.ts` sits at the §3.5 400-line file budget with no
 * headroom, and `ChatBox.tsx` is at its complexity budget — an inline
 * `conversation ?? {}` in the component costs it one branch too many.
 */
import type { ExecutionEventData } from '@/shared/api/sse';

import type { ChatBoxActiveConversation } from './ChatBox.helpers';

/**
 * `ChatBoxProps.extensions.onAgentEvent` — the sink for a host that renders
 * the RUN of a turn and not only its answer (the pipeline editor's flow
 * canvas, whose `onRcvAgentEvent` highlights the executing node and builds
 * the run timeline).
 *
 * Declared here rather than inline in `ChatBox.tsx` for the same reason
 * everything else in this file is: that component is at its §3.5 file-length
 * budget with no headroom.
 *
 * A PASS-THROUGH, not a gate. `useChatStreamTransport` decides which frames
 * are graph frames (`shouldForwardAgentEvent`) and calls this only for those,
 * so a per-token `agent_llm_chunk` never reaches a timeline that has no entry
 * for it. A second filter here would be a copy of that contract, free to
 * drift out of step with it.
 */
export type ChatBoxAgentEventSink = (frame: ExecutionEventData) => void;

/** `ChatBoxProps.conversation` — the conversation on screen, and whether it is still arriving. */
export interface ChatBoxConversationProp {
  readonly active?: ChatBoxActiveConversation;
  readonly isLoading?: boolean;
  /** Promote a newly persisted conversation into the page route and state. */
  readonly onCreated?: ((conversation: { readonly id?: string | number; readonly uuid?: string; readonly activeParticipantId?: string }) => void) | undefined;
}

/** Flattens the `conversation` bundle back into the two values `ChatBox` reads (baseline: the separate `activeConversation`/`isLoadingConversation` props, bundled to stay under the §3.5 component-props budget once `ref` became a prop). */
export function unwrapChatBoxConversation(conversation: ChatBoxConversationProp | undefined): {
  readonly activeConversation: ChatBoxActiveConversation | undefined;
  readonly isLoadingConversation: boolean | undefined;
  readonly onConversationCreated: ChatBoxConversationProp['onCreated'];
} {
  return {
    activeConversation: conversation?.active,
    isLoadingConversation: conversation?.isLoading,
    onConversationCreated: conversation?.onCreated,
  };
}
