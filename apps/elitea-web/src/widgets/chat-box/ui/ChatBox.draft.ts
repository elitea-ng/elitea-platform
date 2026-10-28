import { carryDraft } from '@/shared/lib/composerDrafts';

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
  readonly scratch: readonly unknown[];
}): string | undefined {
  if (params.scratch.some(Boolean) || params.projectId === undefined) return undefined;
  return `chat:${params.projectId}:${params.conversationId ?? 'new'}`;
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
