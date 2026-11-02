import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * The narrowest flow canvas a person can work in. Below it, the editor's side
 * panes give way: at a 1280px window the shell leaves the editor 984px, and
 * with both 20rem panes open the canvas was a ~240px strip whose cards the
 * zoom controls covered.
 */
const MIN_CANVAS_WIDTH_PX = 480;

/*
 * What each layout leaves around the canvas, in px of the editor's content
 * box (`ConfigurationTab`'s `mainContainer`): the General pane (20rem) and its
 * 2rem gap, the split gap (0.625rem), and the chat pane (20rem) — or, folded,
 * its 1.75rem strip plus the editor's 1.5rem right margin.
 */
const BOTH_PANES_OPEN_PX = 320 + 32 + 10 + 320;
const CHAT_FOLDED_PX = 320 + 32 + 10 + 28 + 24;

export interface PaneRoom {
  /** Put on the editor's outer container (the box the panes share). */
  readonly measureRef: (node: HTMLElement | null) => void;
  /** The chat pane should start folded: open, it would squeeze the canvas under the minimum. */
  readonly foldChat: boolean;
  /** The General pane should start folded too: even with the chat folded the canvas is too narrow. */
  readonly foldGeneral: boolean;
}

/** Decides which editor panes fold so the canvas keeps `MIN_CANVAS_WIDTH_PX`. */
export function paneFolds(contentWidth: number): Omit<PaneRoom, 'measureRef'> {
  if (contentWidth <= 0) return { foldChat: false, foldGeneral: false };
  return {
    foldChat: contentWidth - BOTH_PANES_OPEN_PX < MIN_CANVAS_WIDTH_PX,
    foldGeneral: contentWidth - CHAT_FOLDED_PX < MIN_CANVAS_WIDTH_PX,
  };
}

/** Measures the editor's content box and reports which panes fold. */
export function useEditorPaneRoom(): PaneRoom {
  const [width, setWidth] = useState(0);
  const observer = useRef<ResizeObserver | null>(null);

  const measureRef = useCallback((node: HTMLElement | null) => {
    observer.current?.disconnect();
    observer.current = null;
    if (node === null) return;
    // Measured at once, in the commit, so the panes start in their final
    // state rather than opening and then folding a frame later.
    const style = window.getComputedStyle(node);
    setWidth(Math.round(node.clientWidth - (parseFloat(style.paddingLeft) || 0) - (parseFloat(style.paddingRight) || 0)));
    if (typeof ResizeObserver === 'undefined') return;
    const next = new ResizeObserver((entries) => {
      const box = entries[0]?.contentRect;
      if (box !== undefined) setWidth(Math.round(box.width));
    });
    next.observe(node);
    observer.current = next;
  }, []);

  useEffect(() => () => observer.current?.disconnect(), []);

  return { measureRef, ...paneFolds(width) };
}

/**
 * A pane's folded state: the room's verdict until the person toggles it, then
 * theirs. A stacked (small-window) layout never folds.
 */
export function usePaneFold(autoFolded: boolean, isSmallWindow: boolean, onCollapsed: ((collapsed: boolean) => void) | undefined): {
  readonly collapsed: boolean;
  readonly toggle: () => void;
} {
  const [chosen, setChosen] = useState<boolean | null>(null);
  const collapsed = isSmallWindow ? false : (chosen ?? autoFolded);

  useEffect(() => {
    if (isSmallWindow) setChosen(null);
  }, [isSmallWindow]);

  const reported = useRef(false);
  useEffect(() => {
    // Every change, and the first fold; not the first open (nothing changed).
    if (!reported.current && !collapsed) return;
    reported.current = true;
    onCollapsed?.(collapsed);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- report changes of `collapsed` only
  }, [collapsed]);

  const toggle = useCallback(() => setChosen(!collapsed), [collapsed]);
  return { collapsed, toggle };
}
