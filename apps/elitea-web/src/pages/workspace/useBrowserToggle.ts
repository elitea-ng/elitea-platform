/**
 * ⌘⇧B (and the palette's "Toggle browser") for the thread on screen: opens
 * the thread's browser pane as an inspector tab, or closes it when it is the
 * tab being looked at. A new thread (no conversation yet) has no pane: its
 * site data would belong to nobody.
 */
import { useEffect, useRef } from 'react';

import { useBrowserHost, useBrowserPanes } from '@/features/workspace-browser';
import { useDesktopLayout } from '@/widgets/desktop-shell';

export function toggleBrowserPane(threadId: string, close: (threadId: string) => void): void {
  if (threadId === '') return;
  const layout = useDesktopLayout.getState();
  const panes = useBrowserPanes.getState();
  const open = panes.panes[threadId]?.open === true;
  const looking = layout.changesOpen && layout.inspector.local.groups.some((group) => group.active === 'browser') && !layout.inspector.local.closed.includes('browser');
  if (open && looking) {
    close(threadId);
    panes.update(threadId, { open: false, live: false, loading: false });
    return;
  }
  panes.update(threadId, { open: true });
  layout.showInspector('browser');
}

export function useBrowserToggle(conversationId: string): void {
  const { ipc } = useBrowserHost();
  const latest = useRef({ conversationId, ipc });
  useEffect(() => {
    latest.current = { conversationId, ipc };
  });
  useEffect(() => {
    if (ipc === undefined) return undefined;
    useDesktopLayout.getState().extendSession({
      toggleBrowser: () => toggleBrowserPane(latest.current.conversationId, (id) => void latest.current.ipc?.close(id).catch(() => undefined)),
    });
    return () => useDesktopLayout.getState().extendSession({ toggleBrowser: undefined });
  }, [ipc]);
}
