/**
 * `/workspaces?conversation=<thread id>` (a `run_finished` bell row,
 * ADR-0045): find the folder on this computer that holds the thread and open
 * it there. When no folder here holds it, the search is dropped and the
 * folder list stays on screen.
 */
import { useEffect } from 'react';

import { useNavigate, useSearch } from '@tanstack/react-router';

import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

import { findLocalWorkThread } from './localThreadLookup';

export function useOpenThreadFromSearch(ipc: WorkspaceIpc | undefined): void {
  const navigate = useNavigate();
  const search: Record<string, unknown> = useSearch({ strict: false });
  const raw = search['conversation'];
  const threadId = typeof raw === 'string' || typeof raw === 'number' ? String(raw) : '';

  useEffect(() => {
    if (threadId === '' || ipc === undefined) return;
    let live = true;
    void findLocalWorkThread(threadId, ipc).then((workspaceId) => {
      if (!live) return;
      if (workspaceId === null) void navigate({ to: '/workspaces', search: {}, replace: true });
      else void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId }, search: { conversation: threadId }, replace: true });
    });
    return () => {
      live = false;
    };
  }, [threadId, ipc, navigate]);
}
