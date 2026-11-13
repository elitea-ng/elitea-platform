import type { ChatMessage } from '@/features/chat-messages';
import { ROLES } from '@/shared/lib/enums';

/**
 * The `attachment_message` items of a send's uploaded files, in the shape the
 * persisted message carries. The optimistic row used to carry none, so a file
 * attached to a message showed in the composer and vanished from the sent
 * message until the conversation was reloaded (issue 2037).
 */
function optimisticAttachmentItems(attachmentList: readonly unknown[] | undefined): readonly unknown[] {
  return (attachmentList ?? []).flatMap((entry, index) => {
    const { filepath, name } = (entry ?? {}) as { readonly filepath?: unknown; readonly name?: unknown };
    if (typeof filepath !== 'string' || filepath === '') return [];
    return [{ id: -(index + 1), item_type: 'attachment_message', order_index: index + 1, item_details: { filepath, name: typeof name === 'string' ? name : filepath } }];
  });
}
export function buildOptimisticUserMessage(questionId: string, question: string, userParticipant: { id?: string; name?: string; avatar?: string } | undefined, participantId: string | number | undefined, attachmentList?: readonly unknown[]): ChatMessage {
  const attachmentItems = optimisticAttachmentItems(attachmentList);
  return {
    id: questionId,
    role: ROLES.User,
    name: userParticipant?.name ?? '',
    content: question,
    createdAt: new Date().toISOString(),
    ...(attachmentItems.length > 0 ? { messageItems: attachmentItems as ChatMessage['messageItems'] } : {}),
    ...(userParticipant?.avatar !== undefined ? { avatar: userParticipant.avatar } : {}),
    ...(userParticipant?.id !== undefined ? { userId: userParticipant.id } : {}),
    ...(participantId !== undefined ? { participantId: String(participantId) } : {}),
  };
}
