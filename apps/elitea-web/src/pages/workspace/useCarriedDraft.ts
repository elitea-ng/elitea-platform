/**
 * A Local work thread's draft key (`NewChatInput`'s `draftKey`), and the move of a new thread's text to the thread
 * once it got its id (`useCarriedDraft`).
 */
import { useEffect, useLayoutEffect, useRef, type RefObject } from 'react';

import { carryDraft, writeDraft } from '@/shared/lib/composerDrafts';

/** What a thread's unsent text is kept under (`NewChatInput`'s `draftKey`). */
export const threadDraftKey = (workspaceId: string, conversationId: string | undefined): string => `workspace:${workspaceId}:${conversationId === undefined || conversationId === '' ? 'new' : conversationId}`;

/**
 * A new thread got its id without a re-mount (a thread switch re-mounts the session): what the input holds goes on as
 * that thread's draft (`carryDraft`), before the input reads the new key (a layout effect runs before its effects).
 * When the send that started the thread emptied the input in the same commit, the input saves its text from before
 * as `…:new` on the way out: once its effects ran (a parent's run after them), `…:new` is emptied.
 */
export function useCarriedDraft(workspaceId: string, conversationId: string | undefined, input: RefObject<{ getInputContent(): string } | null>): void {
  const adopted = useRef(false);
  const previous = useRef(conversationId);
  useLayoutEffect(() => {
    const before = previous.current;
    previous.current = conversationId;
    adopted.current = (before === undefined || before === '') && conversationId !== undefined && conversationId !== '';
    if (adopted.current) carryDraft(threadDraftKey(workspaceId, conversationId), input.current?.getInputContent() ?? '');
  }, [workspaceId, conversationId, input]);
  useEffect(() => {
    if (!adopted.current) return;
    adopted.current = false;
    if ((input.current?.getInputContent() ?? '') === '') writeDraft(threadDraftKey(workspaceId, ''), '');
  }, [workspaceId, conversationId, input]);
}
