/**
 * Ported from `EliteaUI` `widgets/entity-test-panel/lib/hooks/useTestPanelWidth.hooks.js`.
 * Manages the resizable TestPanel width (320px min, 50% workspace max, 373px default),
 * persisted under the app's `el.` storage namespace so logout wipes it cleanly.
 */
import type { MouseEvent, RefObject } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import { createStorage } from '@/shared/lib/storage';

import { useResizableDrawer } from '../flow-editor/hooks/useResizableDrawer';

export const TEST_PANEL_MIN_WIDTH = 320;
export const TEST_PANEL_DEFAULT_WIDTH = 373;
const TEST_PANEL_MAX_RATIO = 0.5;
const TEST_PANEL_WIDTH_STORAGE_KEY = 'pipeline.testPanel.width';

function clampWidth(width: number, maxWidth: number): number {
  const safeMax = Math.max(TEST_PANEL_MIN_WIDTH, maxWidth);
  return Math.min(safeMax, Math.max(TEST_PANEL_MIN_WIDTH, Math.round(width)));
}

function readStoredWidth(defaultWidth: number, maxWidth: number): number {
  const fallback = clampWidth(defaultWidth, maxWidth);
  try {
    const raw = createStorage('local').get(TEST_PANEL_WIDTH_STORAGE_KEY);
    const stored = raw === null ? Number.NaN : Number(raw);
    return Number.isFinite(stored) && stored >= TEST_PANEL_MIN_WIDTH ? clampWidth(stored, maxWidth) : fallback;
  } catch {
    return fallback;
  }
}

function writeStoredWidth(width: number, maxWidth: number): void {
  try {
    createStorage('local').set(TEST_PANEL_WIDTH_STORAGE_KEY, String(clampWidth(width, maxWidth)));
  } catch {
    // Storage may be unavailable in restricted contexts; keep in-memory width.
  }
}

export function getTestPanelMaxWidth(containerWidth: number): number {
  return Math.max(TEST_PANEL_MIN_WIDTH, Math.floor((containerWidth || 0) * TEST_PANEL_MAX_RATIO));
}

export interface UseTestPanelWidthOptions {
  readonly workspaceRef?: RefObject<HTMLDivElement | null> | undefined;
  readonly onResized?: (() => void) | undefined;
  readonly defaultWidth?: number | undefined;
}

export interface UseTestPanelWidthResult {
  readonly containerRef: RefObject<HTMLDivElement | null>;
  readonly width: number;
  readonly isResizing: boolean;
  readonly handleResizeStart: (event: MouseEvent) => void;
}

export function useTestPanelWidth({
  workspaceRef,
  onResized,
  defaultWidth = TEST_PANEL_DEFAULT_WIDTH,
}: UseTestPanelWidthOptions = {}): UseTestPanelWidthResult {
  const [maxWidth, setMaxWidth] = useState(() => getTestPanelMaxWidth(window.innerWidth));
  const [initialWidth] = useState(() => readStoredWidth(defaultWidth, getTestPanelMaxWidth(window.innerWidth)));

  const { containerRef, drawerWidth, setDrawerWidth, isResizing, handleResizeStart } = useResizableDrawer(
    TEST_PANEL_MIN_WIDTH,
    maxWidth,
    initialWidth,
  );

  const syncMaxWidth = useCallback(() => {
    const max = getTestPanelMaxWidth(workspaceRef?.current?.clientWidth || window.innerWidth);
    setMaxWidth(max);
    setDrawerWidth((current) => Math.min(current, max));
  }, [setDrawerWidth, workspaceRef]);

  useEffect(() => {
    syncMaxWidth();
    const workspaceEl = workspaceRef?.current;
    if (typeof ResizeObserver === 'undefined' || !workspaceEl) {
      window.addEventListener('resize', syncMaxWidth);
      return () => window.removeEventListener('resize', syncMaxWidth);
    }
    const observer = new ResizeObserver(syncMaxWidth);
    observer.observe(workspaceEl);
    return () => observer.disconnect();
  }, [syncMaxWidth, workspaceRef]);

  const wasResizingRef = useRef(false);
  useEffect(() => {
    if (isResizing) {
      wasResizingRef.current = true;
      return;
    }
    if (wasResizingRef.current) {
      wasResizingRef.current = false;
      if (containerRef.current) containerRef.current.style.width = '';
      writeStoredWidth(drawerWidth, maxWidth);
      onResized?.();
    }
  }, [isResizing, drawerWidth, maxWidth, onResized, containerRef]);

  return {
    containerRef,
    width: drawerWidth,
    isResizing,
    handleResizeStart,
  };
}
