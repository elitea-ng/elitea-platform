/**
 * Ported verbatim from `apps/elitea-ui/src/[fsd]/features/pipelines/
 * flow-editor/lib/hooks/useResizableDrawer.hooks.js` (98 lines, unit A2d).
 * Mouse-drag resize handling for the State drawer; no sibling logic
 * dependency beyond A2c's `stateDrawer.constants.ts` for its defaults.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { MAX_DRAWER_WIDTH, MIN_DRAWER_WIDTH } from '../constants/stateDrawer.constants';

export interface UseResizableDrawerResult {
  readonly containerRef: React.RefObject<HTMLDivElement | null>;
  readonly drawerWidth: number;
  readonly setDrawerWidth: React.Dispatch<React.SetStateAction<number>>;
  readonly isResizing: boolean;
  readonly isHoveringHandle: boolean;
  readonly setIsHoveringHandle: (hovering: boolean) => void;
  readonly handleResizeStart: (event: React.MouseEvent) => void;
}

export function useResizableDrawer(
  minWidth: number = MIN_DRAWER_WIDTH,
  maxWidth: number = MAX_DRAWER_WIDTH,
  initialWidth: number = MIN_DRAWER_WIDTH,
): UseResizableDrawerResult {
  const startXRef = useRef(0);
  const startWidthRef = useRef(initialWidth);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const rafIdRef = useRef<number | null>(null);
  const pendingWidthRef = useRef<number | null>(null);

  const [drawerWidth, setDrawerWidth] = useState(initialWidth);
  const [isResizing, setIsResizing] = useState(false);
  const [isHoveringHandle, setIsHoveringHandle] = useState(false);

  const handleResizeStart = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      setIsResizing(true);
      startXRef.current = e.clientX;
      startWidthRef.current = drawerWidth;
    },
    [drawerWidth],
  );

  // Handle resize move with RAF throttling
  const handleResizeMove = useCallback(
    (e: MouseEvent) => {
      if (!isResizing || !containerRef.current) return;

      const deltaX = startXRef.current - e.clientX;
      const newWidth = Math.min(maxWidth, Math.max(minWidth, startWidthRef.current + deltaX));

      // Update DOM directly for immediate visual feedback
      containerRef.current.style.width = `${newWidth}px`;

      // Store pending width for throttled state update
      pendingWidthRef.current = newWidth;

      // Throttle state updates using requestAnimationFrame to reduce re-renders
      if (!rafIdRef.current) {
        rafIdRef.current = requestAnimationFrame(() => {
          if (pendingWidthRef.current !== null) {
            setDrawerWidth(pendingWidthRef.current);
            pendingWidthRef.current = null;
          }
          rafIdRef.current = null;
        });
      }
    },
    [isResizing, minWidth, maxWidth],
  );

  const handleResizeEnd = useCallback(() => {
    // Cancel any pending RAF updates
    if (rafIdRef.current) {
      cancelAnimationFrame(rafIdRef.current);
      rafIdRef.current = null;
    }

    // Set final width from DOM to ensure state matches visual state
    if (containerRef.current && pendingWidthRef.current !== null) {
      setDrawerWidth(pendingWidthRef.current);
      pendingWidthRef.current = null;
    }

    setIsResizing(false);
  }, []);

  // Add and remove event listeners for resize
  useEffect(() => {
    if (isResizing) {
      document.addEventListener('mousemove', handleResizeMove);
      document.addEventListener('mouseup', handleResizeEnd);
      return () => {
        document.removeEventListener('mousemove', handleResizeMove);
        document.removeEventListener('mouseup', handleResizeEnd);
      };
    }
    return undefined;
  }, [isResizing, handleResizeMove, handleResizeEnd]);

  // Clean up RAF on unmount
  useEffect(() => {
    return () => {
      if (rafIdRef.current) {
        cancelAnimationFrame(rafIdRef.current);
      }
    };
  }, []);

  return {
    containerRef,
    drawerWidth,
    setDrawerWidth,
    isResizing,
    isHoveringHandle,
    setIsHoveringHandle,
    handleResizeStart,
  };
}
