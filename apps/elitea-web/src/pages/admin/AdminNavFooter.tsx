/**
 * The admin nav's user footer: avatar + name, opening a menu whose only entry
 * is Logout — the reference's shape.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import LogoutOutlinedIcon from '@mui/icons-material/LogoutOutlined';
import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import ListItemIcon from '@mui/material/ListItemIcon';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { performLogout } from '@/shared/api/auth';
import { t } from '@/shared/i18n';

import { focusRing } from './AdminNavChrome';
import { adminUiUserName } from './adminUiConfig';
import ThemeModeToggle from '@/shared/ui/ThemeModeToggle';

export interface AdminNavFooterProps {
  readonly collapsed: boolean;
}

/**
 * The reference's user row: avatar + name, opening a menu whose only entry is
 * Logout.
 *
 * The reference assigns `window.location.origin + "/auth/logout"`
 * directly. This calls `performLogout()` instead — the same handoff plus the
 * `el.` storage-namespace sweep and the `target_to` that lands the browser on
 * the login screen (see `shared/api/auth/logout.ts`). Re-implementing the raw
 * assignment here would reintroduce the leak §5.4 exists to close, in the one
 * bundle whose sessions are administrative.
 */
export function AdminNavFooter({ collapsed }: AdminNavFooterProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const name = adminUiUserName();

  return (
    <Box sx={{ padding: '0.5rem 0.75rem 1rem' }}>
      {/*
        * The theme toggle lives here, not in the header. At ~7rem it took most
        * of the 13.75rem rail's header row and squeezed "Elitea Admin" down to
        * "El…". The footer is the rail's other chrome slot and has the width.
        * Hidden while collapsed, like every other label in this nav.
        */}
      {!collapsed && (
        <Box sx={{ display: 'flex', justifyContent: 'center', paddingBottom: '0.5rem' }}>
          <ThemeModeToggle />
        </Box>
      )}
      <Box
        component="button"
        type="button"
        data-testid="admin-nav-user-button"
        aria-haspopup="menu"
        aria-expanded={anchorEl !== null}
        onClick={(event) => setAnchorEl(event.currentTarget)}
        sx={(theme: Theme) => ({
          width: '100%',
          display: 'flex',
          alignItems: 'center',
          justifyContent: collapsed ? 'center' : 'flex-start',
          gap: '0.5rem',
          padding: '0.5rem',
          border: 'none',
          appearance: 'none',
          background: 'transparent',
          borderRadius: theme.vars.shape.radiusMd,
          cursor: 'pointer',
          color: theme.vars.palette.text.metrics,
          '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
          ...focusRing(theme),
        })}
      >
        <Avatar
          sx={(theme: Theme) => ({
            width: '1.5rem',
            height: '1.5rem',
            background: theme.vars.palette.background.avatar,
            color: theme.vars.palette.text.secondary,
          })}
        >
          <Typography variant="labelSmall">{name.slice(0, 1).toUpperCase()}</Typography>
        </Avatar>
        {!collapsed && (
          <Typography
            variant="labelSmall"
            sx={{ color: 'inherit', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
          >
            {name}
          </Typography>
        )}
      </Box>
      <Menu
        anchorEl={anchorEl}
        open={anchorEl !== null}
        onClose={() => setAnchorEl(null)}
        anchorOrigin={{ vertical: 'top', horizontal: 'right' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      >
        <MenuItem
          data-testid="admin-nav-logout"
          onClick={() => {
            setAnchorEl(null);
            performLogout();
          }}
        >
          <ListItemIcon sx={{ minWidth: '1rem', marginRight: '0.75rem' }}>
            <LogoutOutlinedIcon sx={{ width: '1rem', height: '1rem' }} />
          </ListItemIcon>
          <ListItemText
            primary={t('pages.admin.nav.logout', 'Logout')}
            slotProps={{ primary: { variant: 'bodyMedium' } }}
          />
        </MenuItem>
      </Menu>
    </Box>
  );
}
