/**
 * Message groups with files, in the shapes the server writes (client contract
 * 1.1): the transcript list row (ConversationMessage with
 * AttachmentMessageItem entries) and the conversation-details group, whose
 * attachment items carry the same seven item_details keys
 * (`attachmentItemDetails`: id, item_type, name, bucket, filepath,
 * attachment_type, content). The image's content holds the `{type: text}`
 * header chunk and the `image_url` data URL the model was given.
 */

export const CONV = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
/** A 1×1 PNG. */
export const PNG_DATA_URL =
  'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';

export function imageItem(id = 501, order = 1) {
  return {
    id,
    item_type: 'attachment_message',
    order_index: order,
    item_details: {
      id,
      item_type: 'attachment_message',
      name: `${CONV}/photo-a1b2.jpg`,
      bucket: 'chat-attachments',
      filepath: `/chat-attachments/${CONV}/photo-a1b2.jpg`,
      attachment_type: 'image',
      content: [
        { type: 'text', text: 'Attached image: photo-a1b2.jpg' },
        { type: 'image_url', image_url: { url: PNG_DATA_URL } },
      ],
    },
  };
}

export function documentItem(id = 502, order = 2, name = 'report-c3d4.pdf') {
  return {
    id,
    item_type: 'attachment_message',
    order_index: order,
    item_details: {
      id,
      item_type: 'attachment_message',
      name: `${CONV}/${name}`,
      bucket: 'chat-attachments',
      filepath: `/chat-attachments/${CONV}/${name}`,
      attachment_type: 'document',
      content: [{ type: 'text', text: `Attached file: ${name}` }],
    },
  };
}

/** A conversation-details message group (the thread's read). */
export function detailsGroup(items: unknown[], text = 'what is in these?') {
  return {
    id: 40,
    uuid: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f',
    author_participant_id: 1,
    created_at: '2026-10-05 10:00:00',
    message_items: [{ id: 500, item_type: 'text_message', order_index: 0, item_details: { content: text } }, ...items],
  };
}

/** A listConversationMessages row (the delta sync's shape). */
export function transcriptRow(items: unknown[]) {
  return {
    id: '40',
    uid: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f',
    conversation_id: '3',
    role: 'user',
    author_participant_id: 1,
    content: 'what is in these?',
    created_at: '2026-10-05T10:00:00Z',
    message_items: items,
  };
}
