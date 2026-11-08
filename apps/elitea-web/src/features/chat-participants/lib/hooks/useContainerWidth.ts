/** The participants layout's width-responsive users cap (split out of `ui/ParticipantsLayout.tsx`, 400-line budget). */
import { useEffect, useState } from 'react';

/**
 * Width-responsive users cap (adversarial review C5-wrapper #9): mirrors
 * old-app `useGetComponentWidth()` via `ResizeObserver`. Extracted to its own
 * hook to keep `ParticipantsLayout`'s own complexity within the §3.5 budget.
 */
export function useContainerWidth(containerRef: React.RefObject<HTMLDivElement | null>): number {
  const [width, setWidth] = useState(0);

  useEffect(() => {
    const node = containerRef.current;
    if (!node || typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setWidth(entry.contentRect.width);
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, [containerRef]);

  return width;
}

/** old-app: `componentWidth <= 200 ? users.slice(0, 3) : users.slice(0, 5)`. */
export function deriveEffectiveMaxVisibleUsers(componentWidth: number, maxVisibleUsers: number): number {
  if (componentWidth > 0 && componentWidth <= 200) return Math.min(3, maxVisibleUsers);
  return maxVisibleUsers;
}
