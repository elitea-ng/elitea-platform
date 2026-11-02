/**
 * Reader for the `?create=1` flag that the sidebar's "New chat" writes.
 *
 * DEFECT this closes. "New chat" (`widgets/chat-rail`'s `useStartChat`)
 * navigates to `/chat` with `search: { create: '1' }`. Nothing read that flag.
 * A user already on `/chat` who clicked "New chat" stayed on the same
 * pathname. The route did not remount. The previous transcript, attachments,
 * model choice and streaming state all stayed on screen. Only a manual page
 * reload started a second chat.
 *
 * The transcript does not live on `/chat`'s page component. It lives inside
 * `ChatBox`, in `useChatBoxData`'s `conversationForSync.chat_history`, beside
 * the pending attachments and the streaming info. No caller above can clear
 * those fields one by one. A remount clears all of them. This hook therefore
 * returns a token. The composition root puts that token in the chat subtree's
 * `key`.
 *
 * The effect also writes the flag back to `'0'`. The function form of `search`
 * is necessary. The button passes a plain object, which replaces the whole
 * search. The flag must return to `'0'`, and must not be deleted. Only then is
 * the next click a real `'0' -> '1'` transition that this effect sees.
 *
 * `to: '/chat'` also drops a `$conversationId` path param. That is correct.
 * From `/chat/<id>` the flag means "leave this conversation and start a new
 * one", and that path already resets on its own.
 *
 * A ref holds `onReset`, and the deps omit it. The caller rebuilds that
 * callback on every render. `ChatWithEditors` memoises it on the objects the
 * editor hooks return, and those objects are new each render. With `onReset`
 * in the deps, one click ran the effect twice and remounted the chat twice.
 */
import { useEffect, useRef, useState } from 'react';

import { useNavigate, useSearch } from '@tanstack/react-router';

interface CreateChatFlagSearch {
  readonly create?: string;
}

/**
 * @param onReset runs before the remount — the composition root closes its
 *   open editors here, the same way the reference app did on this trigger.
 *   A new function identity does not re-trigger the reset.
 * @returns a counter that increases once per "New chat" click.
 */
export function useCreateChatReset(onReset: () => void): number {
  const navigate = useNavigate();
  const { create } = useSearch({ strict: false }) as CreateChatFlagSearch;
  const [resetToken, setResetToken] = useState(0);

  const onResetRef = useRef(onReset);
  useEffect(() => {
    onResetRef.current = onReset;
  });

  useEffect(() => {
    if (create !== '1') return;
    onResetRef.current();
    setResetToken((token) => token + 1);
    void navigate({
      to: '/chat',
      search: (prev: CreateChatFlagSearch) => ({ ...prev, create: '0' }),
      replace: true,
    });
  }, [create, navigate]);

  return resetToken;
}
