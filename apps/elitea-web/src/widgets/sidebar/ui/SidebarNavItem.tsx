import type { ReactNode } from 'react';

import { Link } from '@tanstack/react-router';

import ListItemButton from '@mui/material/ListItemButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

export interface SidebarNavItemProps {
  label: string;
  icon: ReactNode;
  to: string;
  selected: boolean;
  showLabel: boolean;
}

/**
 * Ported from `[fsd]/widgets/sidebar-root/ui/SidebarMenuItem.jsx`, reduced:
 * the old app's "personal-space disabled" tooltip variant
 * (`TooltipForDisablePersonalSpace`/`useDisablePersonalSpace`) reads Redux
 * `state.user.personal_project_id`, which has no source in this app yet
 * (§ "current user identity" gap, `../index.ts`) — every item therefore
 * always renders through the plain collapsed-label tooltip path, never
 * disabled for that reason.
 *
 * A real `<Link>` (R-C1: a real interactive element), not a `Box` +
 * `onClick`.
 */
export function SidebarNavItem({ label, icon, to, selected, showLabel }: SidebarNavItemProps): ReactNode {
  return (
    <Tooltip
      title={showLabel ? '' : label}
      placement="right"
      enterDelay={500}
      enterNextDelay={500}
    >
      <ListItemButton
        component={Link}
        to={to}
        selected={selected}
        sx={(theme: Theme) => ({
          padding: '0.5rem',
          borderRadius: theme.vars.shape.radiusMd,
          height: '2rem',
          // See the same pair on `pages/admin/AdminNav.tsx`'s AdminNavLink.
          // MUI's ListItemButton root sets `flex-grow: 1` for use as a row in a
          // List; these items sit in a COLUMN (`SidebarBody.tsx:142`, `flex: 1`),
          // where that grow makes each one absorb leftover height and the
          // declared `height: '2rem'` stops holding. Only visible when the items
          // do not already fill the rail, which is why it survived here.
          flexGrow: 0,
          flexShrink: 0,
          boxSizing: 'border-box',
          justifyContent: showLabel ? undefined : 'center',
          '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
          '&.Mui-selected': { background: theme.vars.palette.background.button.drawerMenu.selected },
          '&.Mui-selected:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
          // `SidebarMenuItem.jsx`'s `styles.button` paints BOTH the glyph and
          // the label from the selected flag (`icon.fill.secondary`/
          // `text.secondary` when current, `text.metrics` otherwise). The port
          // set neither, so every row — current one included — inherited the
          // same body colour and the rail showed no selected row at all.
          '& path': { fill: selected ? theme.vars.palette.icon.fill.secondary : theme.vars.palette.text.metrics },
          '& span': { color: selected ? theme.vars.palette.text.secondary : theme.vars.palette.text.metrics },
        })}
      >
        <ListItemIcon
          sx={{
            marginRight: showLabel ? '0.5rem' : 0,
            minWidth: '1rem',
            width: '1rem',
            height: '1rem',
          }}
        >
          {icon}
        </ListItemIcon>
        {showLabel && <Typography variant="labelSmall">{label}</Typography>}
      </ListItemButton>
    </Tooltip>
  );
}
