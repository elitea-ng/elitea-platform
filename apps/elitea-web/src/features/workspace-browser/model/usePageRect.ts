/**
 * The page rectangle of a browser pane: the box the host lays its native
 * child webview over. A native view cannot be drawn over, so the page is
 * reported hidden whenever something should cover it — a modal sheet or
 * dialog open anywhere in the window (`aria-modal`, MUI's modal root), the
 * box having no size (its tab not shown, the inspector hidden), or the
 * component unmounting, or the tab popped into a window of its own (the
 * page belongs to the main window). Every change is sent; the host ignores repeats.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import type { PaneRect } from '@/shared/desktop/browserIpc';

/** A modal open in `doc`: any element MUI or the platform marks as one. */
export function modalOpen(doc: Document): boolean {
  return doc.querySelector('[aria-modal="true"], .MuiModal-root:not(.MuiPopover-root):not(.MuiMenu-root)') !== null;
}

export interface PageRectReport {
  readonly rect: PaneRect;
  readonly visible: boolean;
}

function same(a: PageRectReport | null, b: PageRectReport): boolean {
  return a !== null && a.visible === b.visible && a.rect.x === b.rect.x && a.rect.y === b.rect.y && a.rect.width === b.rect.width && a.rect.height === b.rect.height;
}

/**
 * Watches the element given to the returned ref and calls `report` with
 * its rectangle (CSS pixels of the main page) and whether the page may be
 * shown. On unmount it reports hidden once.
 */
export function usePageRect(report: (next: PageRectReport) => void): (node: HTMLElement | null) => void {
  const [node, setNode] = useState<HTMLElement | null>(null);
  const latest = useRef(report);
  const last = useRef<PageRectReport | null>(null);
  useEffect(() => {
    latest.current = report;
  });

  useEffect(() => {
    if (node === null) return undefined;
    const doc = node.ownerDocument;
    const win = doc.defaultView ?? window;
    let frame = 0;
    const measure = (): void => {
      frame = 0;
      const box = node.getBoundingClientRect();
      const rect = { x: Math.round(box.left), y: Math.round(box.top), width: Math.round(box.width), height: Math.round(box.height) };
      // The page is a child of the main window: a tab popped into its own window has no page to show.
      const inMainWindow = doc === globalThis.document;
      const next = { rect, visible: inMainWindow && rect.width > 0 && rect.height > 0 && !modalOpen(doc) };
      if (same(last.current, next)) return;
      last.current = next;
      latest.current(next);
    };
    const schedule = (): void => {
      if (frame === 0) frame = win.requestAnimationFrame(measure);
    };
    measure();
    const resize = new ResizeObserver(schedule);
    resize.observe(node);
    resize.observe(doc.documentElement);
    // A modal mounts and unmounts as a direct child of the body (MUI's portal).
    const modals = new MutationObserver(schedule);
    modals.observe(doc.body, { childList: true, subtree: false, attributes: true, attributeFilter: ['aria-hidden'] });
    win.addEventListener('resize', schedule);
    // Layout moves that resize nothing (the sidebar sliding, a split handle) still move the box.
    const poll = win.setInterval(measure, 500);
    return () => {
      if (frame !== 0) win.cancelAnimationFrame(frame);
      resize.disconnect();
      modals.disconnect();
      win.removeEventListener('resize', schedule);
      win.clearInterval(poll);
      const hidden = { rect: last.current?.rect ?? { x: 0, y: 0, width: 0, height: 0 }, visible: false };
      last.current = null;
      latest.current(hidden);
    };
  }, [node]);

  return useCallback((next: HTMLElement | null) => setNode(next), []);
}
