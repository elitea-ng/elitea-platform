import type { SxProps, Theme } from '@mui/material/styles';

import { typeScale } from '@/shared/brand/typeScale';

/*
 * THE DESKTOP APP'S LOOK (`variant="native"`), matching its own sidebar
 * (`widgets/desktop-shell/ui/rows.tsx`): the secondary-surface tint, 28px
 * rows on the small radius, 16px icons, `labelSmall` text, the drawer-menu
 * hover and selection fills, and section names as captions rather than
 * uppercase banners. No header bar: the dialog is the window's chrome.
 */
export const sentenceCase = (label: string): string => label.charAt(0) + label.slice(1).toLocaleLowerCase();

export const nativeDrawerSx: SxProps<Theme> = (theme) => ({
  width: '13.5rem',
  minWidth: '13.5rem',
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  boxSizing: 'border-box',
  backgroundColor: theme.vars.palette.background.secondary,
  borderRight: `1px solid ${theme.vars.palette.divider}`,
});

export const nativeHeaderSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  minHeight: '2.75rem',
  padding: '0.75rem 1rem 0.25rem',
  boxSizing: 'border-box',
};

export const nativeHeaderTextSx: SxProps<Theme> = (theme) => ({
  ...typeScale(theme.typography.labelMedium),
  fontWeight: 600,
  color: theme.vars.palette.text.secondary,
});

export const nativeMenuContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  overflow: 'auto',
  padding: '0 0.5rem 0.5rem',
};

export const nativeSectionGroupSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
};

export const nativeSectionHeaderSx: SxProps<Theme> = (theme) => ({
  display: 'block',
  ...typeScale(theme.typography.labelSmall),
  fontWeight: 600,
  color: theme.vars.palette.text.metrics,
  padding: '0.75rem 0.5rem 0.25rem',
});

export const nativeItemSx =
  (isActive: boolean): SxProps<Theme> =>
  (theme) => ({
    border: 'none',
    font: 'inherit',
    textAlign: 'start',
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    width: '100%',
    minHeight: '1.75rem',
    padding: '0 0.5rem',
    boxSizing: 'border-box',
    borderRadius: theme.vars.shape.radiusSm,
    cursor: 'default',
    color: theme.vars.palette.text.secondary,
    background: isActive ? theme.vars.palette.background.button.drawerMenu.selected : 'transparent',
    '&:hover': { background: isActive ? undefined : theme.vars.palette.background.button.drawerMenu.hover },
  });

export const nativeIconSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  color: theme.vars.palette.text.secondary,
  '& svg': { fill: 'currentColor', width: '1rem', height: '1rem' },
});

export const nativeItemTextSx: SxProps<Theme> = (theme) => ({
  ...typeScale(theme.typography.labelSmall),
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});
