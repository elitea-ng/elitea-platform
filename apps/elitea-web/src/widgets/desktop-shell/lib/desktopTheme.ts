/**
 * The desktop's component layer: the deployment's brand theme (colours,
 * scheme) re-proportioned the way a native macOS/Windows app is — 13px
 * body, 11px secondary text, compact 22–28px controls, no ripples, no pill
 * buttons, bezelled secondary buttons, labels above inputs instead of
 * floating. It only ever runs in the desktop build (`NativeLook` /
 * `DesktopThemeLayer`), so the shared web theme is untouched.
 */
import type { Theme } from '@mui/material/styles';

import { SheetTransition } from './sheetTransition';

export const SYSTEM_FONT_STACK =
  '-apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI Variable Text", "Segoe UI", system-ui, Roboto, "Helvetica Neue", Arial, sans-serif';

/** Desktop type ladder (px): titles 15, body 13, secondary 11. */
const SIZES: Record<string, number> = {
  headingLarge: 17,
  headingMedium: 15,
  headingSmall: 13,
  labelMedium: 13,
  bodyMedium: 13,
  labelSmall: 11,
  bodySmall: 11,
  subtitle: 11,
};

/** Menus and popovers (ms): on screen at once, a short grow out. */
const MENU_TRANSITION = { enter: 0, exit: 120 } as const;

/** Controls stay between 22 and 28px: small 22, medium 28. */
const CONTROL = { small: 22, medium: 28 } as const;

function withNativeFont(outer: Theme): Record<string, unknown> {
  const brandFamily = outer.typography.fontFamily;
  const typography: Record<string, unknown> = { ...outer.typography, fontFamily: SYSTEM_FONT_STACK };
  for (const [key, value] of Object.entries(outer.typography)) {
    if (typeof value !== 'object' || value === null) continue;
    const entry = value as { fontFamily?: unknown };
    const size = SIZES[key];
    const next: Record<string, unknown> = { ...(value as object) };
    if (entry.fontFamily === brandFamily) next['fontFamily'] = SYSTEM_FONT_STACK;
    if (size !== undefined) {
      next['fontSize'] = `${String(size / 16)}rem`;
      next['letterSpacing'] = key === 'subtitle' ? '0.04em' : '0';
    }
    typography[key] = next;
  }
  // The stock aliases MUI components render internally follow the same ladder.
  const alias: Record<string, string> = { body1: 'bodyMedium', body2: 'bodySmall', caption: 'bodySmall', button: 'labelMedium', h6: 'headingMedium' };
  for (const [stock, ours] of Object.entries(alias)) {
    const source = typography[ours];
    const stockEntry = typography[stock];
    if (typeof source === 'object' && source !== null && typeof stockEntry === 'object' && stockEntry !== null) {
      typography[stock] = { ...stockEntry, fontSize: (source as { fontSize?: unknown }).fontSize };
    }
  }
  return typography;
}

// A flat table of component overrides: its length is the point, not branching.
// oxlint-disable-next-line complexity
export function withDesktopComponents(outer: Theme): Theme {
  const typography = withNativeFont(outer);
  const components = {
    ...outer.components,
    MuiButtonBase: {
      ...outer.components?.MuiButtonBase,
      defaultProps: { ...outer.components?.MuiButtonBase?.defaultProps, disableRipple: true, disableTouchRipple: true },
      styleOverrides: { root: { cursor: 'default' } },
    },
    MuiButton: {
      ...outer.components?.MuiButton,
      defaultProps: { ...outer.components?.MuiButton?.defaultProps, disableRipple: true, disableElevation: true, size: 'small' },
      styleOverrides: {
        root: ({ theme: t, ownerState }: { theme: Theme; ownerState: { variant?: string; color?: string; size?: string } }) => {
          const base = {
            textTransform: 'none',
            borderRadius: 6,
            minHeight: ownerState.size === 'large' ? CONTROL.medium : CONTROL.small,
            minWidth: 0,
            padding: '0 10px',
            fontSize: '0.8125rem',
            fontWeight: 500,
            lineHeight: 1.2,
            boxShadow: 'none',
            transition: 'background-color 80ms linear',
          };
          if (ownerState.variant === 'contained') {
            return { ...base, backgroundColor: t.vars.palette.primary.main, color: t.vars.palette.primary.contrastText, border: '1px solid transparent', '&:hover': { backgroundColor: t.vars.palette.primary.main, filter: 'brightness(1.08)' }, '&:active': { filter: 'brightness(0.92)' } };
          }
          if (ownerState.variant === 'outlined') {
            // The secondary bezel: a hairline and a faint raised fill.
            return {
              ...base,
              color: t.vars.palette.text.secondary,
              border: `1px solid ${t.vars.palette.divider}`,
              backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 4%, transparent)`,
              '&:hover': { backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 8%, transparent)`, border: `1px solid ${t.vars.palette.divider}` },
              '&:active': { backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 12%, transparent)` },
            };
          }
          // Toolbar style: borderless icon + label.
          return {
            ...base,
            color: t.vars.palette.text.secondary,
            padding: '0 6px',
            '&:hover': { backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 8%, transparent)` },
            '&:active': { backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 12%, transparent)` },
          };
        },
        startIcon: { marginRight: 4, marginLeft: 0 },
      },
    },
    MuiIconButton: {
      ...outer.components?.MuiIconButton,
      defaultProps: { ...outer.components?.MuiIconButton?.defaultProps, disableRipple: true, size: 'small' },
      styleOverrides: {
        root: ({ theme: t }: { theme: Theme }) => ({
          borderRadius: 6,
          padding: 3,
          '&:hover': { backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 8%, transparent)` },
        }),
      },
    },
    MuiTooltip: {
      ...outer.components?.MuiTooltip,
      defaultProps: { ...outer.components?.MuiTooltip?.defaultProps, enterDelay: 600, enterNextDelay: 200 },
      styleOverrides: { tooltip: { fontSize: '0.6875rem', padding: '3px 7px', borderRadius: 5 } },
    },
    MuiInputLabel: {
      ...outer.components?.MuiInputLabel,
      defaultProps: { ...outer.components?.MuiInputLabel?.defaultProps, shrink: true },
      styleOverrides: {
        // A label above the field, never floating into its border.
        root: { position: 'static', transform: 'none', fontSize: '0.6875rem', fontWeight: 500, marginBottom: 3, maxWidth: '100%' },
      },
    },
    MuiOutlinedInput: {
      ...outer.components?.MuiOutlinedInput,
      defaultProps: { ...outer.components?.MuiOutlinedInput?.defaultProps, notched: false },
      styleOverrides: {
        root: ({ theme: t }: { theme: Theme }) => ({
          borderRadius: 6,
          fontSize: '0.8125rem',
          minHeight: CONTROL.medium,
          backgroundColor: `color-mix(in srgb, ${t.vars.palette.text.primary} 3%, transparent)`,
          '& .MuiOutlinedInput-notchedOutline': { top: 0, borderColor: t.vars.palette.divider },
          '& .MuiOutlinedInput-notchedOutline legend': { display: 'none' },
          '&.Mui-focused': { outline: `3px solid color-mix(in srgb, ${t.vars.palette.primary.main} 40%, transparent)`, outlineOffset: 0 },
          '&.Mui-focused .MuiOutlinedInput-notchedOutline': { borderColor: t.vars.palette.primary.main, borderWidth: 1 },
        }),
        input: { padding: '4px 8px', height: 'auto' },
        inputSizeSmall: { padding: '2px 8px' },
      },
    },
    // A native menu is on screen the moment it is asked for; MUI's default Grow takes ~250ms each way.
    MuiMenu: {
      ...outer.components?.MuiMenu,
      defaultProps: { ...outer.components?.MuiMenu?.defaultProps, transitionDuration: MENU_TRANSITION },
      styleOverrides: { paper: { borderRadius: 8, marginTop: 4 }, list: { padding: 4 } },
    },
    MuiPopover: {
      ...outer.components?.MuiPopover,
      defaultProps: { ...outer.components?.MuiPopover?.defaultProps, transitionDuration: MENU_TRANSITION },
    },
    MuiMenuItem: {
      ...outer.components?.MuiMenuItem,
      styleOverrides: {
        root: ({ theme: t }: { theme: Theme }) => ({
          minHeight: 24,
          borderRadius: 5,
          padding: '2px 8px',
          fontSize: '0.8125rem',
          '&.Mui-focusVisible, &:hover': { backgroundColor: t.vars.palette.primary.main, color: t.vars.palette.primary.contrastText },
        }),
      },
    },
    MuiListItemButton: {
      ...outer.components?.MuiListItemButton,
      defaultProps: { ...outer.components?.MuiListItemButton?.defaultProps, disableRipple: true },
    },
    MuiSwitch: {
      ...outer.components?.MuiSwitch,
      defaultProps: { ...outer.components?.MuiSwitch?.defaultProps, disableRipple: true, size: 'small' },
    },
    MuiCheckbox: {
      ...outer.components?.MuiCheckbox,
      defaultProps: { ...outer.components?.MuiCheckbox?.defaultProps, disableRipple: true, size: 'small' },
    },
    // Dialogs are sheets: they hang from the toolbar edge, square at the top.
    MuiDialog: {
      ...outer.components?.MuiDialog,
      defaultProps: { ...outer.components?.MuiDialog?.defaultProps, slots: { transition: SheetTransition } },
      styleOverrides: {
        container: { alignItems: 'flex-start' },
        paper: ({ theme: t }: { theme: Theme }) => ({
          margin: 0,
          minWidth: 0,
          maxHeight: 'calc(100% - 24px)',
          borderRadius: '0 0 10px 10px',
          border: `0.5px solid ${t.vars.palette.divider}`,
          borderTop: 'none',
          background: t.vars.palette.background.paper,
          backgroundImage: 'none',
          boxShadow: `0 10px 36px color-mix(in srgb, ${t.vars.palette.text.primary} 28%, transparent)`,
        }),
      },
    },
    MuiBackdrop: {
      ...outer.components?.MuiBackdrop,
      styleOverrides: { root: ({ theme: t }: { theme: Theme }) => ({ backgroundColor: `color-mix(in srgb, ${t.vars.palette.common.black} 30%, transparent)` }) },
    },
    MuiDialogContent: {
      ...outer.components?.MuiDialogContent,
      styleOverrides: { root: { padding: '4px 20px 12px' } },
    },
    MuiDialogActions: {
      ...outer.components?.MuiDialogActions,
      // The default button is the trailing one.
      styleOverrides: { root: { padding: '10px 20px 16px', gap: 8, justifyContent: 'flex-end', '& > :not(style) ~ :not(style)': { marginLeft: 0 } } },
    },
    MuiPaper: {
      ...outer.components?.MuiPaper,
      defaultProps: { ...outer.components?.MuiPaper?.defaultProps, elevation: 0 },
    },
  };
  return { ...outer, typography, components } as unknown as Theme;
}
