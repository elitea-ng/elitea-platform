/**
 * Keeps a chat transcript pinned to its newest line the way a native chat
 * client does: land at the bottom when a conversation opens, jump there when
 * a turn is appended, and FOLLOW the bottom while a reply grows — but only
 * while the reader is at (or near) the bottom. Scrolling up releases the
 * pin; scrolling back down re-engages it.
 *
 * The previous wiring (`ChatMessageList`'s own effect keyed on
 * `[firstMessageId, chatHistory.length]`) only ever fired when the message
 * COUNT changed. A streaming reply grows one row in place — the count does
 * not move — so every token, the thinking accordion, the reply action row
 * and a resized composer pushed the turn below the fold with nothing to
 * pull it back. Size changes are what has to drive the follow, hence the
 * `ResizeObserver` on both the viewport and the content.
 */
import type { RefObject } from 'react';
import { useEffect, useRef } from 'react';

/** How close to the bottom (px) still counts as "at the bottom" — a reader within this distance keeps following the stream. */
const STICK_THRESHOLD = 48;

/** The slice of `SimpleBar`'s instance this hook needs (`ScrollableContainer`'s `ref`). */
interface ScrollHost {
  getScrollElement(): HTMLElement | null;
}

interface StickToBottomOptions {
  readonly scrollRef: RefObject<ScrollHost | null>;
  /** The transcript's content element — its growth is what a streaming reply looks like. */
  readonly contentRef: RefObject<HTMLElement | null>;
  readonly firstId: string | undefined;
  readonly lastId: string | undefined;
  readonly count: number;
}

interface TranscriptShape {
  readonly firstId: string | undefined;
  readonly lastId: string | undefined;
  readonly count: number;
}

function isNearBottom(el: HTMLElement): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= STICK_THRESHOLD;
}

function pinToBottom(el: HTMLElement): void {
  el.scrollTop = el.scrollHeight;
}

/**
 * Whether the transcript gained a NEW last turn (a send, an arriving reply,
 * or a different conversation) — as opposed to an older page prepended by
 * `onScrollToTop` (last id unchanged) or a placeholder id swapped for the
 * server's (count and first id unchanged), neither of which may yank a reader
 * who scrolled up.
 */
function isNewTail(prev: TranscriptShape | null, next: TranscriptShape): boolean {
  if (next.count === 0) return false;
  if (!prev) return true;
  if (prev.lastId === next.lastId) return false;
  return prev.count !== next.count || prev.firstId !== next.firstId;
}

export function useStickToBottom({ scrollRef, contentRef, firstId, lastId, count }: StickToBottomOptions): void {
  const stuckRef = useRef(true);
  const prevRef = useRef<TranscriptShape | null>(null);
  // `ChatMessageList` renders no scroll container for an empty transcript, so
  // the follow wiring has to attach again once the first message mounts it.
  const hasContent = count > 0;

  useEffect(() => {
    const scrollEl = scrollRef.current?.getScrollElement();
    if (!scrollEl) return undefined;
    // Our own pin lands at the bottom, so its scroll event re-asserts `true`;
    // growth fires no scroll event at all, so only the READER can release it.
    const handleScroll = (): void => {
      stuckRef.current = isNearBottom(scrollEl);
    };
    const observer = new ResizeObserver(() => {
      if (stuckRef.current) pinToBottom(scrollEl);
    });
    observer.observe(scrollEl);
    if (contentRef.current) observer.observe(contentRef.current);
    scrollEl.addEventListener('scroll', handleScroll, { passive: true });
    return () => {
      observer.disconnect();
      scrollEl.removeEventListener('scroll', handleScroll);
    };
  }, [scrollRef, contentRef, hasContent]);

  useEffect(() => {
    const next = { firstId, lastId, count };
    const prev = prevRef.current;
    prevRef.current = next;
    if (!isNewTail(prev, next)) return;
    stuckRef.current = true;
    const scrollEl = scrollRef.current?.getScrollElement();
    if (scrollEl) pinToBottom(scrollEl);
  }, [scrollRef, firstId, lastId, count]);
}
