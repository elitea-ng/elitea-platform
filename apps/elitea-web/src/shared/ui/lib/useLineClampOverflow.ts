import { useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';

/**
 * True when the text inside `textRef` is cut by a line clamp or a fixed
 * height: its content is taller than its box.
 *
 * `useTextOverflow` (beside this file) compares widths only, so it sees a
 * one-line ellipsis and misses a multi-line `WebkitLineClamp`. This hook
 * compares heights as well. It re-measures on resize and after the layout
 * settles (the same 50/200 ms delays the width hook uses), and whenever
 * `text` changes.
 *
 * #6861: the agent catalog cut long descriptions and welcome messages with
 * "..." and gave no way to read the rest. A "Show more" control shows only
 * when this hook reports a cut, so a short text gets no dead control.
 */
export function useLineClampOverflow<T extends HTMLElement = HTMLElement>(
  text: unknown,
): { readonly textRef: RefObject<T | null>; readonly isOverflowing: boolean } {
  const textRef = useRef<T | null>(null);
  const [isOverflowing, setIsOverflowing] = useState(false);

  useEffect(() => {
    const element = textRef.current;
    if (!element) return;

    const checkOverflow = (): void => {
      const current = textRef.current;
      if (!current) return;
      setIsOverflowing(current.scrollHeight > current.clientHeight || current.scrollWidth > current.clientWidth);
    };

    checkOverflow();
    const timeouts = [50, 200].map((delay) => setTimeout(checkOverflow, delay));
    const resizeObserver = typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(() => {
      setTimeout(checkOverflow, 10);
    });
    resizeObserver?.observe(element);

    return () => {
      timeouts.forEach((id) => clearTimeout(id));
      resizeObserver?.disconnect();
    };
  }, [text]);

  return { textRef, isOverflowing };
}
