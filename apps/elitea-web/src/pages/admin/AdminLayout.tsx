/**
 * The admin SPA's root layout — the nav beside the routed page (issue #225).
 *
 * The root route used to render a bare `<Outlet/>`. Every one of unit A14's ten
 * pages worked by URL and none of them was linked from anywhere, so an operator
 * who opened `/admin/app/` saw Users and had no way to reach the other nine.
 * This is the component that makes the route tree navigable.
 *
 * `<main>` is a real landmark wrapping the `<Outlet/>`, which the pages'
 * `DrawerPage` (`height: 100%`, its own scroller) fills — so the nav stays put
 * while the page scrolls, rather than scrolling away with it.
 *
 * The layout reads the console capability document (ADR-0059 decision 6,
 * R3.1b, issue 2611) and mounts its two refetch triggers (an admin 403, the
 * window regaining focus). The document's `admin_console_areas` switch (R3.2,
 * issue 2532) chooses the navigation:
 *
 *  - on: the six labelled areas, each with the items the document opens, and
 *    the access-denied page for a typed URL whose item it does not open
 *    (`./AdminConsoleOutlet`);
 *  - off (an upgraded deployment, for one release): the two unlabelled groups
 *    of `./adminNavGroups`, unchanged.
 *
 * The switch rides in the document, so until the document answers the column
 * shows its loading state, and when the read fails, its error with a retry,
 * never an empty navigation. The page itself renders meanwhile: the server
 * answers for its routes either way.
 */
import type { ReactNode } from 'react';

import { Outlet } from '@tanstack/react-router';

import Box from '@mui/material/Box';

import { AdminConsoleOutlet } from './AdminConsoleOutlet';
import { AdminConsoleStatus } from './AdminConsoleStatus';
import { AdminNav } from './AdminNav';
import { visibleConsoleNavAreas } from './adminConsoleAreas';
import { useAdminConsole, useAdminConsoleRefresh } from './api/adminConsoleApi';

export function AdminLayout(): ReactNode {
  const { status, retry, areasEnabled, itemLevel } = useAdminConsole();
  useAdminConsoleRefresh();
  const areas = areasEnabled === true ? visibleConsoleNavAreas(itemLevel) : undefined;
  let navigation: ReactNode;
  if (status !== 'success') navigation = <AdminConsoleStatus status={status} onRetry={retry} />;
  else navigation = <AdminNav areas={areas} />;
  return (
    <Box sx={{ display: 'flex', height: '100vh', width: '100%', overflow: 'hidden' }}>
      {navigation}
      <Box component="main" sx={{ flex: 1, minWidth: 0, height: '100%', overflow: 'hidden' }}>
        {areas === undefined ? <Outlet /> : <AdminConsoleOutlet itemLevel={itemLevel} areas={areas} />}
      </Box>
    </Box>
  );
}
