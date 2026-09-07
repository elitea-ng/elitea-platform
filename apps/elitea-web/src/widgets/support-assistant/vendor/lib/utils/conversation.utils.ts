import type { TMessage, TRawConversation, TRawMessageGroup } from '../types';

const toTimestamp = (value: unknown): number => {
  if (typeof value === 'number') return value < 4102444800 ? value * 1000 : value;
  if (typeof value === 'string') {
    const dt = new Date(value);
    return isNaN(dt.getTime()) ? 0 : dt.getTime();
  }
  return 0;
};

const toMessage = (group: TRawMessageGroup): TMessage => {
  /*
   * A group ADDRESSED to somebody is a question; a group addressed to nobody is
   * an answer. That rule is right, and the key it read was wrong.
   *
   * The reference's socket.io payload names the field `sent_to`. This
   * service's REST transcript names it `sent_to_id`
   * (`repos.ConversationsRepo.ListMessageGroups`), and writes NOTHING under the
   * short name. So `group.sent_to` was always undefined here and every replayed
   * message became an assistant message: reopening a conversation rendered the
   * user's own questions in the answer bubble, with the answers, in one voice.
   *
   * Both names are read. The widget's live turns push their own `role` and
   * never reach this function, which is why the defect survived: it appears
   * only after a reload or a history switch, and only for a conversation that
   * already had messages.
   */
  const addressedTo = group.sent_to_id ?? group.sent_to;
  const role = addressedTo != null ? 'user' : 'assistant';

  let content = '';
  for (const item of group.message_items ?? []) {
    const itemType = item.item_type ?? item.type;
    if (itemType === 'text_message' || itemType === 'text') {
      content = item.item_details?.content ?? item.content ?? '';
      break;
    }
  }

  return {
    id: String(group.uuid ?? group.id ?? ''),
    role,
    content,
    timestamp: toTimestamp(group.created_at_ts ?? group.created_at),
  };
};

export const parseConversationMessages = (conversation: TRawConversation): TMessage[] =>
  (conversation.message_groups ?? []).map(toMessage);

export const generateUUID = (): string => {
  if (typeof crypto !== 'undefined' && crypto.randomUUID) return crypto.randomUUID();

  // Fallback for older browsers
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
    const r = (Math.random() * 16) | 0;
    const v = c === 'x' ? r : (r & 0x3) | 0x8;
    return v.toString(16);
  });
};
