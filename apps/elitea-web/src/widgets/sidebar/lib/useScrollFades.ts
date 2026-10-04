import { useCallback, useLayoutEffect, useRef, useState } from 'react';
import type { RefCallback } from 'react';

export interface ScrollFades {
  /** True when content is hidden above the visible part of the list. */
  readonly showTop: boolean;
  /** True when content is hidden below the visible part of the list. */
  readonly showBottom: boolean;
}

/**
 * Pure: which edges of a scroll box hide content. A 1px tolerance absorbs
 * the fractional `scrollTop` that zoomed and high-DPI screens report at
 * the bottom of a list.
 */
export function scrollFadesOf(element: Pick<HTMLElement, 'scrollTop' | 'scrollHeight' | 'clientHeight'>): ScrollFades {
  const hidden = element.scrollHeight - element.clientHeight;
  if (hidden <= 1) return { showTop: false, showBottom: false };
  return {
    showTop: element.scrollTop > 1,
    showBottom: element.scrollTop < hidden - 1,
  };
}

/**
 * #6712: tells a scroll box which edges to fade, so a list that is longer
 * than its box shows that more items exist above or below.
 *
 * Attach `ref` to the scrolling element and call `onScroll` from its scroll
 * event. The hook also measures when the element mounts, when it resizes,
 * and when `contentKey` changes (pass the item count, so a list that grows
 * while open gets its bottom fade).
 */
export function useScrollFades(contentKey: unknown): {
  readonly ref: RefCallback<HTMLElement>;
  readonly onScroll: () => void;
  readonly fades: ScrollFades;
} {
  const elementRef = useRef<HTMLElement | null>(null);
  const [fades, setFades] = useState<ScrollFades>({ showTop: false, showBottom: false });
  const [element, setElement] = useState<HTMLElement | null>(null);

  const measure = useCallback(() => {
    const current = elementRef.current;
    if (!current) return;
    const next = scrollFadesOf(current);
    setFades((previous) =>
      previous.showTop === next.showTop && previous.showBottom === next.showBottom ? previous : next,
    );
  }, []);

  const ref = useCallback<RefCallback<HTMLElement>>((node) => {
    elementRef.current = node;
    setElement(node);
  }, []);

  useLayoutEffect(() => {
    if (!element) return undefined;
    measure();
    if (typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, [element, contentKey, measure]);

  return { ref, onScroll: measure, fades };
}
