/**
 * Builds `ChatMessageList`'s `continuation` prop group — split out of
 * `ChatBox.tsx` to stay under the §3.5 file-length budget (that file sits
 * right at the 400-line ceiling), same rationale as
 * `ChatBoxInputSlots.tsx`/`ChatBoxPopups.tsx`.
 *
 * A13 (ELITEA-0725, "MCP via Chat Conversation: Secret Field Shows 'New
 * Secret' Shortcut"): `renderAuthModal` fills `ChatContinue`'s slot with the
 * real `McpAuthModal` — see that component's own module doc. Before this,
 * the mid-run "Continue (Auth)" button on an MCP-paused answer called
 * `setShowAuthModal(true)` and rendered nothing: `renderAuthModal` was
 * always `undefined`, so the ONE entry point to authorize an MCP server
 * from inside a live chat turn was a dead button. `no-sideways-features`
 * forbids `features/chat-messages` importing `features/mcps` directly, so
 * the real modal is supplied from here — the one layer above both.
 */
import type { ChatContinueProps, ChatMessageListProps } from '@/features/chat-messages';
import { McpAuthModal } from '@/features/mcps';

type ContinuationProps = NonNullable<ChatMessageListProps['continuation']>;
type AuthModalSlotProps = Parameters<NonNullable<ChatContinueProps['renderAuthModal']>>[0];

export type ChatBoxContinuationHandlers = Pick<
  ContinuationProps,
  'onHitlResume' | 'onContinueMcpExecution' | 'onContinueTokenLimitExecution'
>;

/** `ChatMessageList`'s full `continuation` prop group, `renderAuthModal` included. */
export function buildChatBoxContinuationProps(
  handlers: ChatBoxContinuationHandlers,
  projectId: string | undefined,
): ContinuationProps {
  return {
    ...handlers,
    renderAuthModal: (slotProps: AuthModalSlotProps) => (
      <McpAuthModal
        open={slotProps.open}
        mcpAuthMetadata={slotProps.mcpAuthMetadata}
        serverUrl={slotProps.serverUrl}
        tokenStorageKey={slotProps.tokenStorageKey}
        toolkitId={slotProps.toolkitId}
        toolkitType={slotProps.toolkitType}
        projectId={projectId}
        onClose={slotProps.onClose}
        onCancel={slotProps.onCancel}
      />
    ),
  };
}
