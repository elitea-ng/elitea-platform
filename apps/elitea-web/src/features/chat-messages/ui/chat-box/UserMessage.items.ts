/**
 * Reading a question's raw `messageItems` — split out of `UserMessage.tsx`
 * purely to keep that file under the §3.5 file-length budget.
 */
import type { Attachment } from '@/entities/attachment/model/types';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

export const TEXT_MESSAGE_ITEM_TYPE = 'text_message';
const ATTACHMENT_ITEM_TYPE = 'attachment_message';

/**
 * `messageItems`' declared type (`MessageItemWire[]`) only models `id`/
 * `item_details.content` — the real wire also carries `item_type`/`uuid`,
 * read here the same defensive way `convertMessagesToChatHistory.ts` does.
 */
function getRawMessageItems(messageItems: ChatMessage['messageItems']): ReadonlyArray<Record<string, unknown>> {
  return (messageItems ?? []) as unknown as ReadonlyArray<Record<string, unknown>>;
}

/** Finds the `text_message` item within a message's raw `messageItems` (baseline: `questionItem`). */
export function findQuestionItem(messageItems: ChatMessage['messageItems']): Record<string, unknown> | undefined {
  return getRawMessageItems(messageItems).find((item) => item.item_type === TEXT_MESSAGE_ITEM_TYPE);
}

/** Filters a message's raw `messageItems` down to attachment-typed items (baseline: `attachmentItems`). */
export function findAttachmentItems(messageItems: ChatMessage['messageItems']): readonly Attachment[] {
  const items = getRawMessageItems(messageItems).filter((item) => item.item_type === ATTACHMENT_ITEM_TYPE);
  return items as unknown as readonly Attachment[];
}

/** Reads the text content nested in a `text_message` item's `item_details.content`. */
export function getItemTextContent(item: Record<string, unknown> | undefined): string | undefined {
  const details = item?.item_details as { content?: string } | undefined;
  return details?.content;
}
