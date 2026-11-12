/**
 * Reading a run's conversation out of the message groups `getConversation`
 * embeds when `messages_limit` is set (#1981).
 *
 * A group carries `role` (`user` or `assistant`, from its author's participant
 * row) and `message_items`; the text of a message is its `text_message` item's
 * `item_details.content`. Items other than text (attachments, canvases) have no
 * text of their own and are skipped.
 */
export interface ExchangeEntry {
  readonly id: string;
  readonly role: 'user' | 'assistant';
  readonly text: string;
}

const MAX_PREVIEW = 120;

function textOf(group: Readonly<Record<string, unknown>>): string {
  const items = group['message_items'];
  if (!Array.isArray(items)) return '';
  return items
    .map((item) => {
      if (typeof item !== 'object' || item === null) return '';
      const record = item as Record<string, unknown>;
      if (record['item_type'] !== 'text_message') return '';
      const details = record['item_details'];
      const content = typeof details === 'object' && details !== null ? (details as Record<string, unknown>)['content'] : undefined;
      return typeof content === 'string' ? content : '';
    })
    .filter((text) => text !== '')
    .join('\n\n');
}

function idOf(group: Readonly<Record<string, unknown>>, index: number): string {
  const id = group['uuid'] ?? group['id'];
  return typeof id === 'string' || typeof id === 'number' ? String(id) : String(index);
}

/** The conversation oldest first, with the messages that have no text left out. */
export function exchangeOf(groups: readonly Readonly<Record<string, unknown>>[] | undefined): readonly ExchangeEntry[] {
  return (groups ?? [])
    .map((group, index) => ({
      id: idOf(group, index),
      role: group['role'] === 'user' ? ('user' as const) : ('assistant' as const),
      text: textOf(group).trim(),
    }))
    .filter((entry) => entry.text !== '');
}

/**
 * What a list row says about the run: the first thing the user wrote, on one
 * line and bounded, or `fallback` (the conversation's title) while it is not
 * known or there is none.
 */
export function runPreview(groups: readonly Readonly<Record<string, unknown>>[] | undefined, fallback: string): string {
  const first = exchangeOf(groups).find((entry) => entry.role === 'user');
  if (first === undefined) return fallback;
  const oneLine = first.text.replace(/\s+/g, ' ');
  return oneLine.length > MAX_PREVIEW ? `${oneLine.slice(0, MAX_PREVIEW - 1)}…` : oneLine;
}
