import type { SxProps, Theme } from '@mui/material/styles';

/**
 * `EditApplication.tsx`'s own sx objects, split out purely for the §3.4
 * 400-line budget once #897 added the sticky header styling.
 */
export const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };

// #897 — sticky regardless of WHICH ancestor turns out to be the actual
// scrolling box (this page's own `contentSx`, or an app-shell container
// above it): `position: sticky` pins to the nearest scrolling ancestor
// either way, which a flex `flexShrink: 0` alone does not guarantee.
export const tabBarSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
  minHeight: '3rem',
  position: 'sticky',
  top: 0,
  zIndex: 1,
  backgroundColor: theme.vars.palette.background.default,
});

export const contentSx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '1.5rem' };
