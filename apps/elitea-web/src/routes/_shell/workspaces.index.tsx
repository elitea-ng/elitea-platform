/**
 * `/workspaces` — desktop build only (ADR-0029 D0). In the web build the
 * route still exists in the generated tree (the tree is shared), but its
 * `beforeLoad` sends the visitor to `/chat` and its component is a notice;
 * the real page is not in that bundle (see `pages/workspace/desktopEntry`).
 *
 * `workspaces.index.tsx`, not `workspaces.tsx`: as a parent without an
 * `<Outlet/>` it would swallow `/workspaces/$workspaceId` (see
 * `inventory.index.tsx`).
 *
 * No per-route pending/error components: the router's defaults
 * (`app/router.tsx`) are the same `RoutePending`/`RouteError`, and every
 * per-route copy costs the web build's initial chunk a split-chunk import for
 * a screen the web never renders.
 */
import { createFileRoute } from '@tanstack/react-router';

import { WorkspacesEntry } from '@/pages/workspace/desktopEntry';

import { requireDesktopBuild } from '../-guards/desktopGuard';

export const Route = createFileRoute('/_shell/workspaces/')({
  beforeLoad: requireDesktopBuild,
  component: WorkspacesEntry,
});
