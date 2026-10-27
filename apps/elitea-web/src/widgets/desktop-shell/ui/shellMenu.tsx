/**
 * The sidebar's menus (account, project, appearance, language) drawn at the
 * sidebar's own scale, so a menu reads as part of it: 28px rows, a 16px icon
 * 8px from an 11px label, the sidebar's colours, and the search palette's grey
 * highlight. The desktop theme's `MuiMenuItem` is the native context-menu look
 * (24px rows, accent highlight) and stays that for right-click menus.
 *
 * Each part carries its own style: the theme gate keeps `.Mui*` class
 * selectors out of `sx`, so a doubled `&&` outranks the theme's
 * `.MuiMenuItem-root.Mui-focusVisible` instead of naming it.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import ListSubheader from '@mui/material/ListSubheader';
import MenuItem, { type MenuItemProps } from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

/** One row's look, shared by menu items and the project picker's options. */
export function menuRowSx(theme: Theme, active = false) {
  return {
    display: 'flex',
    alignItems: 'center',
    gap: 1,
    minHeight: '1.75rem',
    paddingX: 1,
    paddingY: 0.25,
    borderRadius: theme.vars.shape.radiusSm,
    ...theme.typography.labelSmall,
    color: theme.vars.palette.text.secondary,
    background: active ? theme.vars.palette.background.button.drawerMenu.selected : undefined,
    '&&:hover, &&:focus-visible': { background: theme.vars.palette.background.button.drawerMenu.selected, color: theme.vars.palette.text.secondary },
  } as const;
}

export function MenuRow(props: MenuItemProps): React.JSX.Element {
  return <MenuItem {...props} sx={(theme: Theme) => menuRowSx(theme)} />;
}

/** The 16px icon column; empty keeps the label on the column's edge. */
export function MenuRowIcon({ children }: { readonly children?: ReactNode }): React.JSX.Element {
  return (
    <Box
      component="span"
      aria-hidden
      sx={(theme: Theme) => ({ display: 'flex', width: '1rem', flexShrink: 0, color: theme.vars.palette.text.metrics, '& svg': { width: '1rem', height: '1rem' } })}
    >
      {children}
    </Box>
  );
}

export function MenuRowText({ children }: { readonly children: ReactNode }): React.JSX.Element {
  return (
    <Typography variant="labelSmall" component="span" noWrap sx={{ flex: 1, minWidth: 0, color: 'inherit' }}>
      {children}
    </Typography>
  );
}

/** Quiet text at the row's end: a shortcut, a version. */
export function MenuRowHint({ children, component = 'span' }: { readonly children: ReactNode; readonly component?: 'span' | 'kbd' }): React.JSX.Element {
  return (
    <Typography variant="bodySmall" component={component} sx={(theme: Theme) => ({ fontFamily: 'inherit', marginInlineStart: 2, color: theme.vars.palette.text.metrics })}>
      {children}
    </Typography>
  );
}

/** A caption over the rows, as the sidebar's section captions; not an item, so keyboard focus skips it. */
export function MenuHeader({ children }: { readonly children: ReactNode }): React.JSX.Element {
  return (
    <ListSubheader
      disableSticky
      sx={(theme: Theme) => ({
        ...theme.typography.labelSmall,
        fontWeight: 600,
        lineHeight: '1.75rem',
        paddingX: 1,
        background: 'transparent',
        color: theme.vars.palette.text.metrics,
        overflow: 'hidden',
        textOverflow: 'ellipsis',
        whiteSpace: 'nowrap',
      })}
    >
      {children}
    </ListSubheader>
  );
}

export function MenuDivider(): React.JSX.Element {
  return <Divider sx={{ '&&': { marginY: 0.5 } }} />;
}
