import { useCallback, useEffect, useRef } from 'react';

const SAFE_INTERNAL_ROUTE_RE = /^\/(?!\/)[^\r\n]*$/;

/**
 * Opens a same-origin create page in a new browser tab (`noopener,noreferrer`) and calls `onReturn`
 * when the user comes back to this tab, so the picker can refresh with what was created there.
 * Refuses non-relative routes (protocol schemes, protocol-relative `//`, CRLF, `..` traversal).
 */
export function useCreateInNewTab(onReturn?: () => void): (route: string) => void {
  const isCreatingRef = useRef(false);
  const onReturnRef = useRef(onReturn);
  onReturnRef.current = onReturn;

  useEffect(() => {
    const onFocus = (): void => {
      if (!isCreatingRef.current) return;
      isCreatingRef.current = false;
      onReturnRef.current?.();
    };
    window.addEventListener('focus', onFocus);
    return () => window.removeEventListener('focus', onFocus);
  }, []);

  return useCallback((route: string) => {
    if (
      !SAFE_INTERNAL_ROUTE_RE.test(route) ||
      route.includes('..') ||
      route.includes('\\') ||
      route.toLowerCase().includes('%2f')
    ) {
      return;
    }
    isCreatingRef.current = true;
    window.open(`${window.location.origin}${route}`, '_blank', 'noopener,noreferrer');
  }, []);
}
