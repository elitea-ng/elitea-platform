import type { ChatMessage } from '@/features/chat-messages';
import { ROLES } from '@/shared/lib/enums';

/** Synthetic rows get ids below zero so they never meet a stored item's id, and a counter so two messages never share one. */
let nextSyntheticItemId = 0;
function syntheticItemId(): number {
  nextSyntheticItemId -= 1;
  return nextSyntheticItemId;
}

/**
 * The items of a just-sent message with files: the text, then one
 * `attachment_message` per uploaded file, shaped like the stored message.
 *
 * Both halves are needed. Readers that find any item (copy, `extractCopyableContent`)
 * use the items INSTEAD of `content`, so attachments alone would copy as
 * "[file]" with the text lost. The text item carries no `uuid`: it names which
 * stored row an edit rewrites, and a made-up one would ask the server to update
 * a row that does not exist (`UserMessage.handleSubmit`). Without attachments
 * the message keeps no items, as before (issue 2037).
 */
function optimisticItems(questionId: string, question: string, attachmentList: readonly unknown[] | undefined): readonly unknown[] {
  const attachments = (attachmentList ?? []).flatMap((entry, index) => {
    const { filepath, name } = (entry ?? {}) as { readonly filepath?: unknown; readonly name?: unknown };
    const path = typeof filepath === 'string' && filepath !== '' ? filepath : undefined;
    const label = typeof name === 'string' && name !== '' ? name : path;
    if (label === undefined) return [];
    return [{
      id: syntheticItemId(),
      uuid: `optimistic:${questionId}:${String(index)}`,
      item_type: 'attachment_message',
      order_index: index + 1,
      item_details: { name: label, ...(path !== undefined ? { filepath: path } : {}) },
    }];
  });
  if (attachments.length === 0) return [];
  return [{ id: syntheticItemId(), item_type: 'text_message', order_index: 0, item_details: { content: question } }, ...attachments];
}

export function buildOptimisticUserMessage(questionId: string, question: string, userParticipant: { id?: string; name?: string; avatar?: string } | undefined, participantId: string | number | undefined, attachmentList?: readonly unknown[]): ChatMessage {
  const items = optimisticItems(questionId, question, attachmentList);
  return {
    id: questionId,
    role: ROLES.User,
    name: userParticipant?.name ?? '',
    content: question,
    createdAt: new Date().toISOString(),
    ...(items.length > 0 ? { messageItems: items as ChatMessage['messageItems'] } : {}),
    ...(userParticipant?.avatar !== undefined ? { avatar: userParticipant.avatar } : {}),
    ...(userParticipant?.id !== undefined ? { userId: userParticipant.id } : {}),
    ...(participantId !== undefined ? { participantId: String(participantId) } : {}),
  };
}
