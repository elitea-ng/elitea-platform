import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';

import { BrandLogoMark } from '@/shared/ui/brand-logo';
import { t } from '@/shared/i18n';

import { NotificationButton } from './NotificationButton';
import { SidebarConnectionDot } from './SidebarConnectionDot';

export interface SidebarHeaderProps {
  collapsed: boolean;
  onToggleCollapsed: () => void;
}

/**
 * Ported from `SidebarBody.jsx`'s sticky header block (the home/toggle
 * button + socket dot, SHELL-012). Renders `LogoMarkIcon` — the gradient
 * brand ORB — which is what production shows here.
 *
 * It used to render `LogoIcon`, the full WORDMARK, as a documented
 * substitution for the mark asset the icon port had not carried over. That
 * substitution did not survive contact with the layout: the wordmark is
 * `0 0 99 20`, so forcing it into a 1.75rem SQUARE scaled 99 units of
 * artwork down to ~28px and the "ELITEA" lettering rendered as an
 * illegible smudge. The mark now exists as its own asset
 * (`icons/svg/logo-mark-icon.svg`), so the substitution is retired.
 *
 * Rendered through `BrandLogoMark` (ADR-0024 WP3): a served pack with a
 * custom `assets.logoMark` shows that image here; the default pack shows the
 * compiled orb.
 *
 * `SidebarConnectionDot` self-degrades to nothing when no
 * `SocketClientContext.Provider` is mounted (see its own header) — always
 * rendered here, never conditionally, so it activates automatically once
 * `app/` wires the provider.
 */
export function SidebarHeader({ collapsed, onToggleCollapsed }: SidebarHeaderProps): ReactNode {
  return (
    <Box
      sx={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: collapsed ? 'center' : 'space-between',
        padding: '0 1rem',
        width: '100%',
        boxSizing: 'border-box',
        // `NAV_BAR_HEIGHT_IN_PX` (`common/constants.js:55`) is 60px, and
        // production measures the rail's first divider at exactly y=60. The
        // port's 3.5rem (56px) sat the logo row 4px short of it.
        minHeight: '3.75rem',
      }}
    >
      <IconButton
        data-testid="sidebar-toggle"
        size="large"
        color="inherit"
        aria-label={t('widgets.sidebar.toggle', 'Toggle sidebar')}
        onClick={onToggleCollapsed}
        // `styles.homeButton`: a 2.75rem square with NO horizontal padding,
        // pulled 0.5rem left while expanded so the mark's optical edge lines
        // up with the nav rows' 1rem inset below it.
        sx={{
          position: 'relative',
          width: '2.75rem',
          height: '2.75rem',
          paddingInline: 0,
          marginLeft: collapsed ? 0 : '-0.5rem',
        }}
      >
        {/* `styles.eliteaIcon` is `fontSize: 1.75rem`, not 2.25rem — the port
            oversized the orb by 8px, which is what pushed the connection dot
            past the button's top-right corner. */}
        <BrandLogoMark style={{ width: '1.75rem', height: '1.75rem' }} />
        <SidebarConnectionDot />
      </IconButton>
      {/*
        * The notification bell belongs in THIS row, to the right of the logo
        * — `SidebarBody.jsx:233` in the old app. It was mounted at the bottom
        * of the rail instead, just above the footer, because the unit that
        * added it did not own this file (see `SidebarBody.tsx`'s own note).
        * The `justifyContent: space-between` above is what puts it at the far
        * end; hidden while collapsed, since the rail is then too narrow for a
        * second control.
        */}
      {!collapsed && <NotificationButton />}
    </Box>
  );
}
