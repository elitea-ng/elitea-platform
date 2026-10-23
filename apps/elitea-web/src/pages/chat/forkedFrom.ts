/**
 * The "Forked from" back-link's data (client contract 1.11): getConversation
 * names the source of a fork in `forked_from`, and states its `name` only
 * when the reader can still see the source. Without a name there is nothing
 * the reader could open, so there is no link.
 */
export interface ForkedFromLinkData {
  /** The source conversation's numeric id, as the `/chat/$conversationId` route takes it. */
  readonly conversationId: string;
  readonly name: string;
}

export function forkedFromOf(details: unknown): ForkedFromLinkData | undefined {
  const value = (details as { readonly forked_from?: unknown } | undefined)?.forked_from;
  if (typeof value !== 'object' || value === null) return undefined;
  const { conversation_id: id, name } = value as { readonly conversation_id?: unknown; readonly name?: unknown };
  if ((typeof id !== 'number' && typeof id !== 'string') || typeof name !== 'string') return undefined;
  return { conversationId: String(id), name };
}
