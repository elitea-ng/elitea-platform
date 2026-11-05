/**
 * Folds the host's `browser://event` into the pane store while a thread is
 * on screen: page loads, titles, refusals, downloads (transcript cards) and
 * the host closing a page to keep four alive.
 */
import { useEffect } from 'react';

import { useBrowserPanes } from './browserPane.store';
import { useBrowserHost } from './ipcContext';

export function useBrowserEvents(): void {
  const { ipc } = useBrowserHost();
  useEffect(() => {
    if (ipc === undefined) return undefined;
    let stop: (() => void) | undefined;
    let gone = false;
    void ipc
      .onEvent((event) => useBrowserPanes.getState().apply(event))
      .then((unsubscribe) => {
        if (gone) unsubscribe();
        else stop = unsubscribe;
      })
      .catch(() => undefined);
    return () => {
      gone = true;
      stop?.();
    };
  }, [ipc]);
}

/** Thread deleted or archived: its page closes and its site data is deleted (the host keeps retrying a store in use). */
export function forgetThreadBrowser(ipc: { forget(threadId: string): Promise<boolean> } | undefined, threadId: string): Promise<void> {
  useBrowserPanes.getState().reset(threadId);
  if (ipc === undefined) return Promise.resolve();
  return ipc.forget(threadId).then(
    () => undefined,
    () => undefined,
  );
}
