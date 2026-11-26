/**
 * The routed page under the six-area navigation (ADR-0059 decision 6, R3.2,
 * issue 2532).
 *
 * A typed URL whose item the capability document does not open shows the
 * access-denied page for that item instead of the page. The index route
 * renders Users; a caller the document does not open Users to lands on the
 * first item it opens instead of a refusal. An item that spans pages
 * ("Schedules and tasks", Usage, "Budgets and alerts") shows its pages under
 * a strip that links them (`./AdminItemTabs`).
 *
 * An old Configuration anchor of the LLM gateway editor
 * (`/configuration#llm_proxy`, R3.5a, issue 2535) opens the page its tab
 * became.
 *
 * Only a KNOWN "not open" turns a page away. While the document is unknown,
 * for a route no item owns, and for an item the document cannot list yet
 * (`available: false`), the page renders and the server answers for itself:
 * a failed read is never an answer about access.
 */
import type { ReactNode } from 'react';

import { Navigate, Outlet, useRouterState } from '@tanstack/react-router';

import Box from '@mui/material/Box';

import { AdminItemAccessDenied } from './AdminItemAccessDenied';
import { AdminItemTabs, itemTabStripFor } from './AdminItemTabs';
import {
  adminConsoleNavItems,
  consoleItemOpen,
  consoleNavItemForRoute,
  type AdminConsoleNavArea,
  type ConsoleItemLevel,
} from './adminConsoleAreas';
import { gatewayAnchorRoute } from './configurationAnchors';

export interface AdminConsoleOutletProps {
  readonly itemLevel: ConsoleItemLevel;
  /** The areas the document opens, as the navigation shows them. */
  readonly areas: readonly AdminConsoleNavArea[];
}

/** A page of an item that spans pages under the strip that links them; any other page as it is. */
function RoutedPage({ leaf }: { readonly leaf: string | undefined }): ReactNode {
  const strip = itemTabStripFor(leaf);
  if (strip === undefined || leaf === undefined) return <Outlet />;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <AdminItemTabs strip={strip} current={leaf} />
      <Box sx={{ flex: 1, minHeight: 0 }}>
        <Outlet />
      </Box>
    </Box>
  );
}

/** The first item the document opens. */
function landingItem(areas: readonly AdminConsoleNavArea[]): AdminConsoleNavArea['items'][number] | undefined {
  return areas.flatMap((area) => area.items)[0];
}

export function AdminConsoleOutlet({ itemLevel, areas }: AdminConsoleOutletProps): ReactNode {
  const route = useRouterState({
    select: (state) => {
      const ids: readonly string[] = state.matches.map((match) => match.routeId);
      return {
        itemId: consoleNavItemForRoute(ids)?.id,
        index: ids.includes('/'),
        leaf: ids.at(-1),
        hash: state.location.hash,
      };
    },
    structuralSharing: true,
  });
  const moved = route.leaf === '/configuration' ? gatewayAnchorRoute(route.hash) : undefined;
  if (moved !== undefined) return <Navigate to={moved} hash="" replace />;
  const entry = adminConsoleNavItems().find((candidate) => candidate.id === route.itemId);
  if (entry === undefined || consoleItemOpen(entry, itemLevel) !== false) return <RoutedPage leaf={route.leaf} />;
  const first = landingItem(areas);
  if (route.index && first !== undefined) return <Navigate to={first.path} replace />;
  return <AdminItemAccessDenied itemId={entry.id} label={entry.label} />;
}
