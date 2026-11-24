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
 * window regaining focus). `consoleNavigation` is the switch R3.2 (issue
 * 2532) sets from the document's `admin_console_areas` flag: with it on, the
 * column shows the document's loading and error states instead of a
 * navigation; with it off (the only state until R3.2 lands) the navigation
 * keeps reading `window.admin_ui_config` and nothing visible changes.
 */
import type { ReactNode } from 'react';

import { Outlet } from '@tanstack/react-router';

import Box from '@mui/material/Box';

import { AdminConsoleStatus } from './AdminConsoleStatus';
import { AdminNav } from './AdminNav';
import { useAdminConsole, useAdminConsoleRefresh } from './api/adminConsoleApi';

export interface AdminLayoutProps {
  /** Navigation driven by the capability document (R3.2's flag); off by default. */
  readonly consoleNavigation?: boolean;
}

export function AdminLayout({ consoleNavigation = false }: AdminLayoutProps = {}): ReactNode {
  const { status, retry } = useAdminConsole();
  useAdminConsoleRefresh();
  return (
    <Box sx={{ display: 'flex', height: '100vh', width: '100%', overflow: 'hidden' }}>
      {consoleNavigation && status !== 'success' ? <AdminConsoleStatus status={status} onRetry={retry} /> : <AdminNav />}
      <Box component="main" sx={{ flex: 1, minWidth: 0, height: '100%', overflow: 'hidden' }}>
        <Outlet />
      </Box>
    </Box>
  );
}
