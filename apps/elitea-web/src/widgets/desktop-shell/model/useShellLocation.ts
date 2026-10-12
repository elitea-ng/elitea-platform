/** Where the shell is: an Elitea page, or a folder (and which of its threads). */
import { useRouterState } from '@tanstack/react-router';

export interface ShellLocation {
  pathname: string;
  /** Set on `/workspaces/$workspaceId`. */
  workspaceId: string | undefined;
  /** The open thread; '' on a new thread or off a folder. */
  conversationId: string;
  /** `/workspaces` or a folder: the workspace-first part of the app. */
  inFolders: boolean;
}

const WORKSPACE_PATH = /^\/workspaces\/([^/]+)\/?$/;

function parseShellLocation(pathname: string, search: Record<string, unknown>): ShellLocation {
  const match = WORKSPACE_PATH.exec(pathname);
  const raw = search['conversation'];
  const conversationId = typeof raw === 'string' || typeof raw === 'number' ? String(raw) : '';
  const workspaceId = match?.[1] === undefined ? undefined : decodeURIComponent(match[1]);
  return {
    pathname,
    workspaceId,
    conversationId: workspaceId === undefined ? '' : conversationId,
    inFolders: workspaceId !== undefined || pathname === '/workspaces' || pathname === '/workspaces/',
  };
}

export function useShellLocation(): ShellLocation {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const search = useRouterState({ select: (state) => state.location.search as Record<string, unknown> });
  return parseShellLocation(pathname, search);
}
