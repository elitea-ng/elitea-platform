/**
 * Right-click → the OS's own context menu. `useContextMenu()` returns a
 * function to call from an `onContextMenu` handler: it stops the browser's
 * menu, pops the native one with `items`, and runs the `run` callback of the
 * item chosen. Without a host (a plain browser tab) it does nothing and
 * leaves the browser's own menu alone.
 */
import { useCallback, type MouseEvent } from 'react';

import type { ContextMenuItem } from '@/shared/desktop/appEvents';

import { useAppIpc } from './appIpcContext';

/** One entry the caller describes: what it says and does. */
export type ContextAction = { id: string; label: string; enabled?: boolean; run: () => void } | { separator: true };

const toItem = (action: ContextAction): ContextMenuItem => {
  if ('separator' in action) return { separator: true };
  return action.enabled === undefined ? { id: action.id, label: action.label } : { id: action.id, label: action.label, enabled: action.enabled };
};

export function useContextMenu(): (event: MouseEvent, actions: readonly ContextAction[]) => void {
  const ipc = useAppIpc();
  return useCallback(
    (event, actions) => {
      if (ipc === undefined) return;
      // Over a text selection the system's own menu (Copy, Look Up) is the right one.
      const selection = window.getSelection();
      if (selection !== null && !selection.isCollapsed && selection.toString().trim() !== '') return;
      event.preventDefault();
      event.stopPropagation();
      ipc.contextMenu(actions.map(toItem)).then(
        (chosen) => {
          if (chosen === null) return;
          const picked = actions.find((action): action is Extract<ContextAction, { id: string }> => 'id' in action && action.id === chosen);
          picked?.run();
        },
        () => undefined,
      );
    },
    [ipc],
  );
}
