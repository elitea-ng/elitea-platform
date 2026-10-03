/**
 * The admin SPA's left-hand navigation (issue #225).
 *
 * `./adminNavItems.ts` owns the items, the permission filtering and the
 * active-route rule, and its header records every deliberate departure from the
 * reference sidebar (`frontends/admin_ui/frontend/src/components/Layout/
 * Sidebar.jsx`). This file is the chrome around them: header, two groups either
 * side of a divider with the second bottom-aligned, the user footer, and the
 * collapse toggle that hangs off the right edge.
 *
 * (The model is `adminNavItems.ts`, not `adminNav.ts`: macOS and Windows
 * filesystems are case-insensitive, so `adminNav.ts` and `AdminNav.tsx` collide
 * during module resolution — vite resolved the import to the wrong one of the
 * pair, and the failure surfaced as a nonsense "cannot resolve @mui/icons"
 * error attributed to a file with the other's extension.)
 *
 * ## The socket-connected dot is deliberately NOT here
 *
 * The reference paints a green/red dot on the logo, driven by Redux
 * `settings.socketConnected`, itself written by socket.io lifecycle listeners.
 * The ported equivalent, `widgets/sidebar`'s `SidebarConnectionDot`, now reads
 * the health of the app's SSE channels (`shared/api/sse`'s `useRealtimeStatus`)
 * instead of socket.io, and this bundle subscribes to no live channel, so it
 * would only ever be `idle` (rendered as nothing). A dot that is always the
 * same colour, or one wired to a client that never connects, tells an operator
 * the server is unreachable when it is not, which is worse than telling them
 * nothing. There is no admin-side liveness signal to drive it from honestly, so
 * it is omitted rather than faked; the day the admin bundle subscribes to a live
 * channel that reports into `useRealtimeStatus`, this header is where it goes.
 *
 * ## Accessibility
 *
 * The whole thing is one `<nav>` landmark with an accessible name. Items are
 * TanStack `<Link>`s rendered through `ListItemButton`, i.e. real anchors —
 * tab-reachable, activatable with Enter, and openable in a new tab — not `Box`
 * + `onClick` (which is what the reference uses, and which no keyboard can
 * reach). The active item carries `aria-current="page"` so it is announced, not
 * merely tinted. Focus is visible in both schemes through an explicit
 * `:focus-visible` outline; `AdminNav.test.tsx` pins the contrast ratios of the
 * label/background token pairs in BOTH schemes, because the E2E axe fixture has
 * `color-contrast` disabled and would not catch a regression.
 */
import type { ReactNode } from 'react';
import { Fragment, useCallback, useEffect } from 'react';

import { Link, useRouterState } from '@tanstack/react-router';

import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import Box from '@mui/material/Box';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { AdminNavDivider, focusRing } from './AdminNavChrome';
import { AdminNavFooter } from './AdminNavFooter';
import { AdminNavHeader } from './AdminNavHeader';
import { activeAdminNavItemId, visibleAdminNavGroups, type AdminNavItem } from './adminNavItems';
import {
  COLLAPSED_SIDE_BAR_WIDTH_REM,
  SIDE_BAR_WIDTH_REM,
} from '@/shared/ui/layout/sidebarWidth';

import {
  readPersistedAdminNavCollapsed,
  useAdminNavCollapsedStore,
  writePersistedAdminNavCollapsed,
} from './adminNavCollapsed';

/**
 * The rail's two widths — the SAME two the main app's rail uses.
 *
 * These were `13.75rem`/`3.75rem` (220/60px) while the main rail was 208/72,
 * and the commit that introduced `sidebarWidth.ts` to unify them only ever
 * changed one side: this file kept its own literals, so the two rails still
 * disagreed and the shared rem exports had no consumer at all. The dead-code
 * gate is what surfaced that — the constants were unused because the wiring
 * was never finished, not because they were surplus.
 *
 * Both rails now read the baseline's own numbers (216/64,
 * `apps/elitea-ui/src/common/constants.js:51,53`).
 */
const ADMIN_NAV_WIDTH = SIDE_BAR_WIDTH_REM;
const ADMIN_NAV_COLLAPSED_WIDTH = COLLAPSED_SIDE_BAR_WIDTH_REM;

export function AdminNav(): ReactNode {
  const collapsed = useAdminNavCollapsedStore((state) => state.collapsed);
  const setCollapsed = useAdminNavCollapsedStore((state) => state.setCollapsed);

  // One-shot hydration on mount — `adminNavCollapsed.ts`'s header explains why
  // the read cannot happen at the store's module scope.
  useEffect(() => {
    setCollapsed(readPersistedAdminNavCollapsed());
  }, [setCollapsed]);

  const toggle = useCallback(() => {
    const next = !useAdminNavCollapsedStore.getState().collapsed;
    writePersistedAdminNavCollapsed(next);
    setCollapsed(next);
  }, [setCollapsed]);

  /*
   * The router's own answer to "what is active", selected down to a primitive.
   *
   * `state.matches` carries the leaf match AND every ancestor, which is what
   * makes a nested route highlight its section without any pathname string
   * surgery — the reference's `isActiveTab` compared the last path segment to
   * the item id, so `/users/123` matched nothing. Reducing to the id inside
   * `select` also keeps the subscription on a stable primitive, so an unrelated
   * router-state change cannot re-render the nav.
   */
  const activeId = useRouterState({
    select: (state) => activeAdminNavItemId(state.matches.map((match) => match.routeId)),
  });

  const groups = visibleAdminNavGroups();
  const width = collapsed ? ADMIN_NAV_COLLAPSED_WIDTH : ADMIN_NAV_WIDTH;

  return (
    <Box
      component="nav"
      aria-label={t('pages.admin.nav.ariaLabel', 'Admin navigation')}
      data-testid="admin-nav"
      sx={(theme: Theme) => ({
        position: 'relative',
        width,
        minWidth: width,
        maxWidth: width,
        transition: 'width 0.2s ease-in-out, min-width 0.2s ease-in-out, max-width 0.2s ease-in-out',
        display: 'flex',
        flexDirection: 'column',
        height: '100%',
        boxSizing: 'border-box',
        borderRight: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        background: theme.vars.palette.background.sideBar,
      })}
    >
      <AdminNavHeader collapsed={collapsed} onToggle={toggle} />

      {groups.map((group, index) => (
        <Fragment key={group.id}>
          {index > 0 && <AdminNavDivider />}
          <Box
            sx={{
              display: 'flex',
              flexDirection: 'column',
              gap: '0.5rem',
              padding: '0.5rem 0.75rem',
              boxSizing: 'border-box',
              maxWidth: '100%',
              // The reference bottom-aligns its second group. Growing the FIRST
              // group's container is what pushes everything after it down; the
              // divider then sits directly above the bottom group, as it does
              // there. When permissions leave only ONE group, it grows instead
              // and the footer stays pinned to the bottom either way.
              ...(index === 0 ? { flex: 1 } : {}),
            }}
          >
            {group.items.map((item) => (
              <AdminNavLink
                key={item.id}
                item={item}
                active={item.id === activeId}
                collapsed={collapsed}
              />
            ))}
          </Box>
        </Fragment>
      ))}

      <AdminNavDivider />
      <AdminNavFooter collapsed={collapsed} />

      <Box
        component="button"
        type="button"
        data-testid="admin-nav-collapse-toggle"
        aria-label={
          collapsed
            ? t('pages.admin.nav.expand', 'Expand navigation')
            : t('pages.admin.nav.collapse', 'Collapse navigation')
        }
        aria-expanded={!collapsed}
        onClick={toggle}
        sx={(theme: Theme) => ({
          position: 'absolute',
          top: '3rem',
          right: '-0.75rem',
          width: '1.5rem',
          height: '1.5rem',
          padding: 0,
          appearance: 'none',
          borderRadius: theme.vars.shape.radiusPill,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          background: theme.vars.palette.background.secondary,
          color: theme.vars.palette.text.secondary,
          display: 'flex',
          justifyContent: 'center',
          alignItems: 'center',
          cursor: 'pointer',
          zIndex: 1,
          ...focusRing(theme),
        })}
      >
        {collapsed ? (
          <ChevronRightIcon sx={{ width: '1rem', height: '1rem' }} />
        ) : (
          <ChevronLeftIcon sx={{ width: '1rem', height: '1rem' }} />
        )}
      </Box>
    </Box>
  );
}

interface AdminNavLinkProps {
  readonly item: AdminNavItem;
  readonly active: boolean;
  readonly collapsed: boolean;
}

function AdminNavLink({ item, active, collapsed }: AdminNavLinkProps): ReactNode {
  const Icon = item.icon;
  return (
    <Tooltip
      title={collapsed ? item.label : ''}
      placement="right"
      enterDelay={500}
      enterNextDelay={500}
    >
      <ListItemButton
        component={Link}
        to={item.path}
        data-testid={`admin-nav-item-${item.id}`}
        selected={active}
        // Announced, not merely tinted. The reference conveys "active" through
        // colour alone.
        aria-current={active ? 'page' : undefined}
        sx={(theme: Theme) => ({
          padding: '0.5rem',
          height: '2rem',
          // MUI's own ListItemButton root sets `flex-grow: 1` — it is built for
          // a row inside a List. Here the items sit in a COLUMN whose first
          // group is `flex: 1` (so the second group bottom-aligns), and on the
          // column's main axis that grow makes every item absorb a share of the
          // leftover height. `height: '2rem'` above then never holds: flex-grow
          // resolves the main size and the declared height is only a basis, so
          // the rail rendered as evenly-spread items with large gaps between
          // them rather than a compact 2rem list.
          //
          // Pinned here rather than in the container: the container's `flex: 1`
          // is deliberate and load-bearing for the bottom-aligned group.
          flexGrow: 0,
          flexShrink: 0,
          borderRadius: theme.vars.shape.radiusMd,
          boxSizing: 'border-box',
          justifyContent: collapsed ? 'center' : 'flex-start',
          color: active ? theme.vars.palette.text.secondary : theme.vars.palette.text.metrics,
          '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
          '&.Mui-selected': { background: theme.vars.palette.background.button.drawerMenu.selected },
          '&.Mui-selected:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
          ...focusRing(theme),
        })}
      >
        <ListItemIcon
          sx={{
            color: 'inherit',
            minWidth: '1rem',
            width: '1rem',
            height: '1rem',
            marginRight: collapsed ? 0 : '0.5rem',
          }}
        >
          <Icon sx={{ width: '1rem', height: '1rem' }} />
        </ListItemIcon>
        {!collapsed && (
          <Typography
            variant="labelSmall"
            sx={{ color: 'inherit', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}
          >
            {item.label}
          </Typography>
        )}
      </ListItemButton>
    </Tooltip>
  );
}

