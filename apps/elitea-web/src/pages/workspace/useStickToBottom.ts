/**
 * Keeps a scrolling transcript on its newest line while it grows — as long
 * as the person is at the bottom. Scrolling up to read stops the follow;
 * scrolling back down resumes it. A reopened thread starts at its end.
 */
import { useEffect, useRef } from 'react';

/** How close to the end still counts as "at the bottom", in px. */
const SLACK_PX = 48;

export function useStickToBottom<S extends HTMLElement, C extends HTMLElement>(): { scrollRef: React.RefObject<S | null>; contentRef: React.RefObject<C | null> } {
  const scrollRef = useRef<S>(null);
  const contentRef = useRef<C>(null);
  const following = useRef(true);

  useEffect(() => {
    const scroller = scrollRef.current;
    const content = contentRef.current;
    if (scroller === null || content === null || typeof ResizeObserver === 'undefined') return undefined;
    const onScroll = (): void => {
      following.current = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight <= SLACK_PX;
    };
    const observer = new ResizeObserver(() => {
      if (following.current) scroller.scrollTop = scroller.scrollHeight;
    });
    scroller.addEventListener('scroll', onScroll, { passive: true });
    observer.observe(content);
    return () => {
      scroller.removeEventListener('scroll', onScroll);
      observer.disconnect();
    };
  }, []);

  return { scrollRef, contentRef };
}
