import { carryDraft, rememberDraftAlias } from '@/shared/lib/composerDrafts';

/**
 * What the chat composer's unsent text belongs to (`NewChatInput`'s
 * `content.draftKey`, kept by `shared/lib/composerDrafts`): the
 * conversation, or "a new chat in this project" before one exists. Type in
 * one conversation, open another, come back: the text is there; a send
 * empties the input and so drops the draft.
 *
 * None on the agent and pipeline editors' test chats (`scratch`: any of
 * `isAgentsPage`, `editorTest` set): their transcript is scratch, and a
 * "new" test chat would otherwise share the chat page's "new" draft.
 */
export function chatDraftKey(params: {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | number | undefined;
  /**
   * The conversation the page is opening (its URL's id). It wins over the
   * loaded `conversationId`: while the conversation loads that one is not
   * there yet (or still the previous conversation's), so the key would read
   * "a new chat" and then flip to the conversation's own, and text typed in
   * between was discarded by the flip. The URL's id never flips.
   */
  readonly pendingConversationId?: string | number | undefined;
  readonly scratch: readonly unknown[];
}): string | undefined {
  if (params.scratch.some(Boolean) || params.projectId === undefined) return undefined;
  return `chat:${params.projectId}:${params.pendingConversationId ?? params.conversationId ?? 'new'}`;
}

/**
 * Once a conversation has loaded, both of its ids are known: its uuid (what a
 * deep link or notification carries) and its numeric id. They name ONE draft,
 * so a deep link by either form finds the text; the numeric id's key is the
 * one the draft is kept under.
 */
export function rememberConversationDraftIds(params: {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly scratch: readonly unknown[];
}): void {
  const { conversationId, conversationUuid } = params;
  if (conversationId === undefined || conversationUuid === undefined) return;
  const canonical = chatDraftKey({ projectId: params.projectId, conversationId, scratch: params.scratch });
  const alias = chatDraftKey({ projectId: params.projectId, conversationId: conversationUuid, scratch: params.scratch });
  if (canonical !== undefined && alias !== undefined) rememberDraftAlias(alias, canonical);
}

type Created = { readonly id?: string | number };

/**
 * `onCreated` that first carries the composer's text to the new conversation's
 * draft (`carryDraft`), so a chat created while text was typed keeps it.
 */
export function withDraftCarry<C extends Created>(
  onCreated: ((conversation: C) => void) | undefined,
  params: { readonly projectId: string | number | undefined; readonly scratch: readonly unknown[]; readonly text: () => string },
): (conversation: C) => void {
  return (conversation) => {
    const to = conversation.id === undefined ? undefined : chatDraftKey({ projectId: params.projectId, conversationId: conversation.id, scratch: params.scratch });
    const text = params.text();
    if (to !== undefined && text.trim() !== '') carryDraft(to, text);
    onCreated?.(conversation);
  };
}
