/**
 * The inspector's panel windows: a tab opened in a native window of its own.
 *
 * The window is an empty same-origin document (`about:blank#elitea-panel=<tab>`,
 * `shared/desktop/panelWindows.ts`) that the main window fills: the frame
 * portals the tab into it, so the tab keeps the page's React tree — the
 * host IPC of Local work, the chat's native transport and queries, every
 * handler — and is as live as it is in the panel, without a second app, route
 * or sign-in in the window. Its styles are the main window's, mirrored
 * (`lib/mirrorStyles.ts`): synced when the main window's styles change, not
 * on a timer. Its `target="_blank"` links leave for the system browser like
 * the main window's (`shared/desktop/externalLinks.ts`). The host names the window `panel-<tab>` and
 * remembers its size and position under that label.
 *
 * Closing the window returns the tab to the panel. A tab the page stops
 * offering (the person went to another kind of page) closes its window; a
 * moment's absence (the thread remounting) does not.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import type { AppIpc } from '@/shared/desktop/appEvents';
import { installExternalLinksIn } from '@/shared/desktop/externalLinks';
import type { InspectorTabId, InspectorTabInfo } from '@/shared/desktop/inspectorSlot';
import { panelWindowLabel, panelWindowUrl } from '@/shared/desktop/panelWindows';
import { t } from '@/shared/i18n';

import { mirrorStyles, watchStyles, type StyleMirror } from '../lib/mirrorStyles';
import { useDesktopLayout } from './desktopLayout.store';

/** How long a tab may be missing before its window closes (a thread switch remounts the page's tabs). */
const ABSENCE_MS = 1500;
/** How often an open window is checked for having closed or replaced its document (no event says so). */
const POLL_MS = 250;
const ROOT_ID = 'elitea-panel-root';

interface PanelWindowRoot {
  /** Where the frame renders the window's content. */
  readonly root: HTMLElement;
  /** The window's body: the tab's popovers and menus open there. */
  readonly body: HTMLElement;
}

export interface PanelWindows {
  readonly roots: ReadonlyMap<InspectorTabId, PanelWindowRoot>;
  readonly open: (tab: InspectorTabId) => void;
  readonly focus: (tab: InspectorTabId) => void;
  readonly close: (tab: InspectorTabId) => void;
}

interface Entry {
  readonly win: Window;
  doc: Document;
  mirror: StyleMirror;
  /** Removes the external-link handler from `doc`. */
  unlink: () => void;
}

type OpenWindow = (url: string, name: string, features: string) => Window | null;

const windowTitle = (info: InspectorTabInfo | undefined): string | undefined =>
  info === undefined ? undefined : info.subject === '' ? info.title : `${info.title} — ${info.subject}`;

/** Gets the window's document ready to be filled: its root element, title, link handling and the main window's styles. */
function attach(win: Window, title: string | undefined): { doc: Document; mirror: StyleMirror; unlink: () => void; root: PanelWindowRoot } {
  const doc = win.document;
  if (title !== undefined) doc.title = title;
  let root = doc.getElementById(ROOT_ID);
  if (root === null) {
    root = doc.createElement('div');
    root.id = ROOT_ID;
    doc.body.append(root);
  }
  const mirror = mirrorStyles(document, doc);
  mirror.sync();
  return { doc, mirror, unlink: installExternalLinksIn(doc), root: { root, body: doc.body } };
}

function detach(entry: Entry): void {
  entry.mirror.dispose();
  entry.unlink();
}

export function usePanelWindows(tabs: readonly InspectorTabInfo[], appIpc: AppIpc | undefined, openWindow: OpenWindow = (url, name, features) => window.open(url, name, features)): PanelWindows {
  const entries = useRef(new Map<InspectorTabId, Entry>());
  const [roots, setRoots] = useState<ReadonlyMap<InspectorTabId, PanelWindowRoot>>(new Map());
  const infos = useRef(tabs);
  infos.current = tabs;
  /** Stops the style watch and the poll; set while at least one window is open. */
  const unwatch = useRef<(() => void) | null>(null);
  const titleOf = useCallback((tab: InspectorTabId): string | undefined => windowTitle(infos.current.find((info) => info.id === tab)), []);

  const forget = useCallback((tab: InspectorTabId) => {
    const entry = entries.current.get(tab);
    if (entry === undefined) return;
    entries.current.delete(tab);
    detach(entry);
    if (entries.current.size === 0) {
      unwatch.current?.();
      unwatch.current = null;
    }
    setRoots((current) => {
      const next = new Map(current);
      next.delete(tab);
      return next;
    });
    useDesktopLayout.getState().setTabPopped(tab, false);
  }, []);

  const close = useCallback(
    (tab: InspectorTabId) => {
      const entry = entries.current.get(tab);
      forget(tab);
      entry?.win.close();
    },
    [forget],
  );

  const focus = useCallback(
    (tab: InspectorTabId) => {
      const entry = entries.current.get(tab);
      if (entry === undefined) return;
      // WebKit ignores `focus()` on another window; the host raises it.
      if (appIpc === undefined) entry.win.focus();
      else appIpc.focusWindow(panelWindowLabel(tab)).catch(() => entry.win.focus());
    },
    [appIpc],
  );

  /**
   * While a window is open: the main window's style changes are mirrored into
   * every window, and a poll notices a window closed (its close button, ⌘W)
   * or its document replaced under us (attached afresh, title included).
   */
  const watch = useCallback((): (() => void) => {
    const syncAll = (): void => entries.current.forEach((entry) => entry.mirror.sync());
    const stopStyles = watchStyles(document, syncAll);
    const timer = window.setInterval(() => {
      entries.current.forEach((entry, tab) => {
        if (entry.win.closed) {
          forget(tab);
          return;
        }
        if (entry.win.document === entry.doc) return;
        detach(entry);
        const attached = attach(entry.win, titleOf(tab));
        entry.doc = attached.doc;
        entry.mirror = attached.mirror;
        entry.unlink = attached.unlink;
        setRoots((current) => new Map(current).set(tab, attached.root));
      });
    }, POLL_MS);
    return () => {
      stopStyles();
      window.clearInterval(timer);
    };
  }, [forget, titleOf]);

  const open = useCallback(
    (tab: InspectorTabId) => {
      if (entries.current.has(tab)) {
        focus(tab);
        return;
      }
      const win = openWindow(panelWindowUrl(tab), `elitea-panel-${tab}`, 'popup,width=420,height=640');
      if (win === null) {
        useDesktopLayout.getState().setNotice(t('desktop.inspector.windowFailed', 'That tab could not open in a window of its own.'));
        return;
      }
      const attached = attach(win, titleOf(tab));
      entries.current.set(tab, { win, doc: attached.doc, mirror: attached.mirror, unlink: attached.unlink });
      unwatch.current ??= watch();
      setRoots((current) => new Map(current).set(tab, attached.root));
      useDesktopLayout.getState().setTabPopped(tab, true);
    },
    [focus, openWindow, titleOf, watch],
  );

  // Titles follow the tab ("Changes — fix CI"); a tab the page no longer offers closes its window.
  useEffect(() => {
    const timers: number[] = [];
    entries.current.forEach((entry, tab) => {
      const info = tabs.find((candidate) => candidate.id === tab);
      const title = windowTitle(info);
      if (title !== undefined) {
        if (entry.doc.title !== title) entry.doc.title = title;
        return;
      }
      timers.push(window.setTimeout(() => close(tab), ABSENCE_MS));
    });
    return () => timers.forEach((timer) => window.clearTimeout(timer));
  }, [tabs, close]);

  // The windows go with the frame (sign-out, the main window reloading).
  useEffect(() => {
    const closeAll = (): void => Array.from(entries.current.keys()).forEach((tab) => close(tab));
    window.addEventListener('pagehide', closeAll);
    return () => {
      window.removeEventListener('pagehide', closeAll);
      closeAll();
    };
  }, [close]);

  return { roots, open, focus, close };
}
