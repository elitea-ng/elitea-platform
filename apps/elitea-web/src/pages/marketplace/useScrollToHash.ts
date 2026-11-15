/**
 * Scrolls to the `#hash` section of a listing page once the page has rendered
 * it. The listing loads after the route settles, so the browser's own anchor
 * jump finds nothing: the origin banner's "See the versions" link depends on
 * this. Call it from the view that renders the sections, so it runs only when
 * they exist.
 */
import { useEffect } from 'react';

import { useLocation } from '@tanstack/react-router';

export function useScrollToHash(): void {
  const hash = useLocation({ select: (location) => location.hash });
  useEffect(() => {
    if (hash === '') return;
    document.getElementById(hash)?.scrollIntoView?.();
  }, [hash]);
}
