import type { RefObject } from 'react';
import { useEffect, useState } from 'react';

/**
 * Width of a live element, in px, tracked with a `ResizeObserver`.
 *
 * The baseline reads `window.innerWidth` (`useGetWindowWidth`) and compares it
 * against the artifacts table's `hideBelow` thresholds — which is wrong by the
 * width of the two side panels, so the Type column survived to a narrower TABLE
 * than the thresholds intend. Measuring the table itself is the same rule
 * applied to the box it was written for; `jsdom` has no `ResizeObserver`, hence
 * the guard and the `fallback` (tests render at the default width).
 */
export function useElementWidth(ref: RefObject<HTMLElement | null>, fallback = 1200): number {
  const [width, setWidth] = useState(fallback);

  useEffect(() => {
    const element = ref.current;
    if (element === null || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry !== undefined) setWidth(entry.contentRect.width);
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, [ref]);

  return width;
}
