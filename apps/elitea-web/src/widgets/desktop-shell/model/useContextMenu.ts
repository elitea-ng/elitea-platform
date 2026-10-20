/**
 * Right-click → the OS's own context menu. `useContextMenu()` returns a
 * function to call from an `onContextMenu` handler: it stops the browser's
 * menu, pops the native one with `actions`, and runs the `run` callback of
 * the item chosen. Without a host (a plain browser tab) it does nothing and
 * leaves the browser's own menu alone. When the host refuses the menu (too
 * many items, an OS error) the refusal is logged and the person is told, so a
 * right-click never silently does nothing.
 */
import { useCallback, type MouseEvent } from 'react';

import type { ContextMenuItem } from '@/shared/desktop/appEvents';
import { hostLog } from '@/shared/desktop/diagnostics';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { useAppIpc } from './appIpcContext';
import { useDesktopLayout } from './desktopLayout.store';

/** An entry of a submenu. */
type ContextLeaf = { id: string; label: string; enabled?: boolean; run: () => void };

/** One entry the caller describes: what it says and does. A submenu holds one level of entries. */
export type ContextAction = ContextLeaf | { separator: true } | { label: string; enabled?: boolean; submenu: readonly ContextLeaf[] };

const leaf = (action: ContextLeaf): { id: string; label: string; enabled?: boolean } =>
  action.enabled === undefined ? { id: action.id, label: action.label } : { id: action.id, label: action.label, enabled: action.enabled };

const toItem = (action: ContextAction): ContextMenuItem => {
  if ('separator' in action) return { separator: true };
  if ('submenu' in action) {
    return action.enabled === undefined
      ? { label: action.label, children: action.submenu.map(leaf) }
      : { label: action.label, enabled: action.enabled, children: action.submenu.map(leaf) };
  }
  return leaf(action);
};

function find(actions: readonly ContextAction[], id: string): ContextLeaf | undefined {
  for (const action of actions) {
    if ('separator' in action) continue;
    if ('submenu' in action) {
      const inside = action.submenu.find((child) => child.id === id);
      if (inside !== undefined) return inside;
    } else if (action.id === id) return action;
  }
  return undefined;
}

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
          if (chosen !== null) find(actions, chosen)?.run();
        },
        (error: unknown) => {
          const refusal = toWorkspaceIpcError(error);
          hostLog('warn', `context menu refused (${refusal.code}): ${refusal.message}`, 'ui');
          useDesktopLayout.getState().setNotice(t('desktop.menu.refused', 'The menu could not be shown: {{message}}', { message: refusal.message }));
        },
      );
    },
    [ipc],
  );
}
