/** `/workspaces/$workspaceId` — one workspace's agent session. Desktop build only; see `workspaces.index.tsx`. */
import { createFileRoute } from '@tanstack/react-router';

import { WorkspaceSessionEntry } from '@/pages/workspace/desktopEntry';

import { requireDesktopBuild } from '../-guards/desktopGuard';
import { RouteError, RoutePending } from '../-ui/RouteStatus';

export const Route = createFileRoute('/_shell/workspaces/$workspaceId')({
  beforeLoad: requireDesktopBuild,
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: WorkspaceSessionEntry,
});
