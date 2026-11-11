/**
 * Small, independent utility hooks for the chat-messages cluster.
 *
 * These are NOT a port of `apps/elitea-ui/src/components/Chat/hooks.js` —
 * that ~1600-line file is a live socket-event-to-message-state pipeline
 * (`useChatSocket`, `useChatMessageSyncSocket`, `useSocketEvents`,
 * `useStopStreaming`, `useCtrlEnterKeyEventsHandler`, and six
 * `useChatXxxSocket` variants) that determines each message's real-time
 * `isLoading`/`isStreaming`/`task_id`/`references`/`hitlInterrupt` state as
 * agent events stream in off the socket. `usePrevious`/
 * `useOutsideClick` below are unrelated, self-contained utilities with no
 * dependency on that pipeline.
 *
 * That socket pipeline is NOT reachable from this app's current
 * message-rendering path: `widgets/chat-box/ui/hooks/useChatBoxData.ts`
 * only emits `chat_predict` (see `ChatBox.tsx`'s `handleSend`) and never
 * registers an `on(...)` listener; live/derived state instead comes from
 * `entities/conversation/lib/hooks/useChatStreaming.ts`, which its own doc
 * comment describes as "pure Redux-[now Zustand-]derived local state ...
 * not a socket hook". So the HITL/exception/streaming props
 * `ApplicationAnswer`/`ChatMessageList` now accept and render (this fix
 * round, items 1-5) are correctly *shaped* to receive that data, but have no
 * live producer yet — porting the real socket pipeline is future work for
 * whichever unit builds real-time chat streaming (C6 territory), not this
 * fix round, and does not belong under a false "ported from hooks.js"
 * label.
 */
import { useEffect, useRef } from 'react';

/** `usePrevious` — returns the previous value of a prop/state. */
export function usePrevious<T>(value: T): T | undefined {
  const ref = useRef<T | undefined>(undefined);

  useEffect(() => {
    ref.current = value;
  }, [value]);

  return ref.current;
}

/** `useOutsideClick` — detects clicks outside a ref'd element. */
export function useOutsideClick<T extends HTMLElement>(ref: React.RefObject<T>, handler: () => void): void {
  useEffect(() => {
    const listener = (event: MouseEvent | TouchEvent) => {
      if (!ref.current || ref.current.contains(event.target as Node)) return;
      handler();
    };

    document.addEventListener('mousedown', listener);
    document.addEventListener('touchstart', listener);

    return () => {
      document.removeEventListener('mousedown', listener);
      document.removeEventListener('touchstart', listener);
    };
  }, [ref, handler]);
}
