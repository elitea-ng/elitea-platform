import type { ChatMessage } from '@/features/chat-messages';

function extractMessageItemText(rawItem: unknown): string {
  const raw = rawItem as Record<string, unknown>;
  const details = raw.item_details as Record<string, unknown> | undefined;
  if (raw.item_type === 'canvas_message') return ((details?.latest_version as Record<string, unknown> | undefined)?.canvas_content as string | undefined) ?? '';
  if (raw.item_type === 'attachment_message') return `[${(details?.name as string | undefined) ?? 'Attachment'}]`;
  return (details?.content as string | undefined) ?? '';
}
export function extractCopyableContent(message: ChatMessage): string {
  if (message.messageItems?.length) return message.messageItems.map(extractMessageItemText).join(', ');
  return message.content || '';
}
