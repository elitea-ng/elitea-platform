/**
 * The routed page under the six-area navigation (ADR-0059 decision 6, R3.2,
 * issue 2532).
 *
 * A typed URL whose item the capability document does not open shows the
 * access-denied page for that item instead of the page. The index route
 * renders Users; a caller the document does not open Users to lands on the
 * first item it opens instead of a refusal. The one "Schedules and tasks"
 * item shows its two pages under a strip that links them.
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
import { AdminSchedulesTasksTabs } from './AdminSchedulesTasksTabs';
import {
  adminConsoleNavItems,
  consoleItemOpen,
  consoleNavItemForRoute,
  type AdminConsoleNavArea,
  type ConsoleItemLevel,
} from './adminConsoleAreas';

export interface AdminConsoleOutletProps {
  readonly itemLevel: ConsoleItemLevel;
  /** The areas the document opens, as the navigation shows them. */
  readonly areas: readonly AdminConsoleNavArea[];
}

/** `/schedules` and `/tasks` under the strip that links them; any other page as it is. */
function RoutedPage({ leaf }: { readonly leaf: string | undefined }): ReactNode {
  if (leaf !== '/schedules' && leaf !== '/tasks') return <Outlet />;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <AdminSchedulesTasksTabs current={leaf} />
      <Box sx={{ flex: 1, minHeight: 0 }}>
        <Outlet />
      </Box>
    </Box>
  );
}

/**
 * The first item the document opens. The TEMPORARY Roles link (an unavailable
 * item, remove in R3.6, issue 2537) is passed over while any other item is
 * open: it would otherwise land the security auditor on a page it can only
 * read and that is not under the read-only wrapper yet.
 */
function landingItem(areas: readonly AdminConsoleNavArea[]): AdminConsoleNavArea['items'][number] | undefined {
  const items = areas.flatMap((area) => area.items);
  return items.find((entry) => entry.available) ?? items[0];
}

export function AdminConsoleOutlet({ itemLevel, areas }: AdminConsoleOutletProps): ReactNode {
  const route = useRouterState({
    select: (state) => {
      const ids: readonly string[] = state.matches.map((match) => match.routeId);
      return { itemId: consoleNavItemForRoute(ids)?.id, index: ids.includes('/'), leaf: ids.at(-1) };
    },
    structuralSharing: true,
  });
  const entry = adminConsoleNavItems().find((candidate) => candidate.id === route.itemId);
  if (entry === undefined || consoleItemOpen(entry, itemLevel) !== false) return <RoutedPage leaf={route.leaf} />;
  const first = landingItem(areas);
  if (route.index && first !== undefined) return <Navigate to={first.path} replace />;
  return <AdminItemAccessDenied itemId={entry.id} label={entry.label} />;
}
