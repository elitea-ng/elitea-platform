/**
 * The optimistic shape of an answer the user asked to regenerate.
 *
 * A regeneration replaces the answer IN PLACE: the server deletes the old
 * text and streams new text into the same message. It does NOT delete a
 * canvas (#1098). A canvas holds what the user edited, so the server keeps it
 * (with all its saved versions) on the answer, before the new text.
 *
 * This patch does the same on the client. It clears the words and keeps the
 * canvas items, so the canvas stays visible and openable while the new answer
 * streams. Before #1098 the patch kept every item, so a stale text item could
 * hide the new streamed `content` (AnswerContent shows `content` only when no
 * text item states it).
 */
import type { ChatMessage } from '@/features/chat-messages';

const CANVAS_MESSAGE_ITEM_TYPE = 'canvas_message';

function isCanvasItem(item: unknown): boolean {
  return typeof item === 'object' && item !== null && (item as Record<string, unknown>)['item_type'] === CANVAS_MESSAGE_ITEM_TYPE;
}

export const regeneratingPatch = (item: ChatMessage): ChatMessage => ({
  ...item,
  content: '',
  toolActions: [],
  exception: undefined,
  references: [],
  isLoading: true,
  isStreaming: true,
  createdAt: new Date().toISOString(),
  ...(item.messageItems !== undefined ? { messageItems: item.messageItems.filter(isCanvasItem) } : {}),
});
