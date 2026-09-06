import type { SxProps, Theme } from '@mui/material/styles';

/**
 * `AgentPipelineVersionSelector.tsx`'s style constants, split into their own
 * file purely to keep that component under the §3.5 400-line file-length
 * budget — the same split `ToolCard.styles.ts` already documents for the
 * sibling tool card. Every value is carried verbatim from the component;
 * the port notes on the individual constants travel with them.
 */

export const contentWrapperSx: SxProps<Theme> = { display: 'inline-flex', alignItems: 'center', width: 'auto', mt: 0, position: 'relative' };

export const selectorSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  gap: '0.25rem',
  cursor: 'pointer',
  padding: '0rem',
  '&:hover .agents-version-text': { color: theme.vars.palette.text.createButton },
  '&:hover .agents-dropdown-icon': { color: theme.vars.palette.text.createButton },
});

export const warningIconSx: SxProps<Theme> = (theme: Theme) => ({ width: '0.875rem', height: '0.875rem', color: theme.vars.palette.warning.main, mr: '0.25rem', flexShrink: 0 });

const versionTextBaseSx = {
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  maxWidth: '7.5rem',
  flexShrink: 1,
} as const;

export const versionTextSx: SxProps<Theme> = (theme: Theme) => ({ ...versionTextBaseSx, color: theme.vars.palette.text.primary });
export const versionTextInvalidSx: SxProps<Theme> = (theme: Theme) => ({ ...versionTextBaseSx, color: theme.vars.palette.warning.main });

const dropdownIconBaseSx = { width: '1rem', height: '1rem', transition: 'transform 0.2s ease-in-out', flexShrink: 0 } as const;
export const dropdownIconSx: SxProps<Theme> = (theme: Theme) => ({ ...dropdownIconBaseSx, color: theme.vars.palette.text.primary });
export const dropdownIconInvalidSx: SxProps<Theme> = (theme: Theme) => ({ ...dropdownIconBaseSx, color: theme.vars.palette.warning.main });

/** Passed via `Menu`'s own `slotProps.paper.sx` (a real, documented MUI slot) instead of an outer `sx`-based `'& .MuiPaper-root'` selector — R-T6 bans `.Mui<Component>-<slot>` selectors outside `shared/brand/mui-overrides/`. */
export const menuPaperSx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.secondary,
  boxShadow: theme.vars.palette.boxShadow.default,
  minWidth: '15rem',
  maxWidth: '17.5rem',
  maxHeight: '12.5rem',
  overflow: 'hidden',
});

/** Baseline's own `'& .MuiList-root'` override on the menu (R-T6-banned selector shape here) — carried via `Menu`'s `slotProps.list.sx` instead, mirroring `menuPaperSx`'s slot-based approach above. Without this the list has no scrollable region of its own and overflowing version entries are silently clipped by `menuPaperSx`'s `overflow: 'hidden'`. */
export const menuListSx: SxProps<Theme> = {
  padding: '0 0 0.25rem',
  maxHeight: '11.5rem',
  overflowY: 'auto',
};

export const versionHeaderSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  padding: '0.25rem 0.75rem 0.25rem 1.25rem',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  minHeight: '1.75rem',
  marginBottom: '0.25rem',
});

export const versionHeaderTitleSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.default, textTransform: 'uppercase' });

const menuItemBaseSx = { padding: '0.5rem 1.25rem', minHeight: '2.5rem', display: 'flex', alignItems: 'center', justifyContent: 'space-between' } as const;
export const menuItemSx: SxProps<Theme> = (theme: Theme) => ({ ...menuItemBaseSx, color: theme.vars.palette.text.secondary, cursor: 'pointer' });
export const selectedMenuItemSx: SxProps<Theme> = (theme: Theme) => ({ ...menuItemBaseSx, fontWeight: 500, color: theme.vars.palette.text.secondary, background: theme.vars.palette.background.conversation.selected, cursor: 'default' });

export const selectedCheckIconSx: SxProps<Theme> = (theme: Theme) => ({ width: '1rem', height: '1rem', color: theme.vars.palette.text.secondary, ml: 1 });

export const refreshIconStyle = { width: '0.75rem', height: '0.75rem' };

/** #147 — the trailing cluster of a version row: the "Default" marker, then the selected check. */
export const rowEndSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem', flexShrink: 0 };

export const defaultMarkerSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.default, textTransform: 'uppercase' });

/** `justifyContent: 'flex-start'` overrides `menuItemBaseSx`'s `space-between`: this row is an icon+label pair, not a label with a trailing marker. */
export const setDefaultItemSx: SxProps<Theme> = (theme: Theme) => ({
  ...menuItemBaseSx,
  justifyContent: 'flex-start',
  gap: '0.5rem',
  color: theme.vars.palette.text.secondary,
  borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  marginTop: '0.25rem',
});

export const setDefaultIconSx: SxProps<Theme> = { width: '1rem', height: '1rem', flexShrink: 0 };

/** #147 — the delete item. The same icon+label row as `setDefaultItemSx`, without a second separator: the two items are one command block at the foot of the menu, and a rule between them would read as two unrelated groups. */
export const deleteItemSx: SxProps<Theme> = (theme: Theme) => ({
  ...menuItemBaseSx,
  justifyContent: 'flex-start',
  gap: '0.5rem',
  color: theme.vars.palette.text.secondary,
});

export const deleteIconSx: SxProps<Theme> = { width: '1rem', height: '1rem', flexShrink: 0 };
