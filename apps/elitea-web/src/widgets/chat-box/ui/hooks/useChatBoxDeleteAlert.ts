/**
 * The chat's delete confirmation (single message / clear-all). Baseline:
 * `useDeleteMessageAlert.hooks.js`'s confirm-then-delete gating. The chat
 * deletes first, then tells the host.
 */
import { useDeleteMessageAlert } from '@/features/chat-messages';

interface DeleteHandlers {
  deleteAnswer: (messageId: string) => Promise<unknown>;
  clearChat: () => Promise<unknown>;
}

export function useChatBoxDeleteAlert(
  handlers: DeleteHandlers,
  onDeleteAnswer: ((messageId: string) => void) | undefined,
  onDeleteAllMessages: (() => void) | undefined,
): ReturnType<typeof useDeleteMessageAlert> {
  return useDeleteMessageAlert({
    onDeleteChatMessage: async (messageId) => {
      await handlers.deleteAnswer(messageId);
      onDeleteAnswer?.(messageId);
    },
    onDeleteAllChatMessages: async () => {
      await handlers.clearChat();
      onDeleteAllMessages?.();
    },
  });
}
