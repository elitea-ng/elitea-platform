import type { SxProps, Theme } from '@mui/material/styles';

/**
 * `ToolCard.tsx`'s style functions, split into their own file purely to
 * keep `ToolCard.tsx` itself under the §3.5 400-line file-length budget —
 * same reason `entities/toolkit`'s promotion split `consts.js` across three
 * files. Ported faithfully from `ToolCard.jsx`'s bottom-of-file
 * `toolCardStyles` object (old-app lines 662-817), one property per
 * exported function/const here.
 *
 * DEVIATIONS (both R-T10/R-T5-forced, not stylistic choices):
 *  - Every ad-hoc `borderRadius` literal (`'0.5rem'`, `'50%'`, `'1.3125rem'`)
 *    is replaced with `theme.vars.shape.radiusMd`/`radiusPill` — R-T10 bans
 *    literal radii outright (`tools/lint-rules/rules/ad-hoc-radius.mjs`,
 *    no exception for `0` either). `radiusPill` (`shape.radiusPill: 9999`,
 *    `shared/brand/buildTheme.ts`'s own comment) is this app's token for a
 *    true circle/pill — exactly the `MuiButton` `icon`/`maxi` variants'
 *    problem this file also has for `entityIconImageSx`/`attachmentButtonSx`.
 *    `cardHeaderSx`'s "rounded top only when expanded" shorthand
 *    (`'0.5rem 0.5rem 0 0'`) becomes four longhand corner properties
 *    (conditionally spread) instead of a shorthand string, since a
 *    computed multi-corner STRING would either fail the same literal check
 *    or require unit-suffixing a `theme.vars.*` value whose runtime shape
 *    (raw number vs. `var(--el-…)` CSS-variable string, under `cssVariables`
 *    mode) this file has no reason to assume.
 *  - The baseline's three `!important`s (`entityIconSx`'s three
 *    `2.125rem !important`s, `attachmentButtonSx`'s `minWidth`/`padding`)
 *    are dropped, not waived: they existed to beat `MuiIconButton`'s
 *    `styleOverrides.root` default geometry
 *    (`shared/brand/mui-overrides/MuiIconButton.ts`), but MUI's `sx` prop is
 *    documented to already win over `styleOverrides` at the CSS layer —
 *    verified by this sub-unit's own component tests still rendering the
 *    correct sizes without `!important`.
 */

export const cardContainerSx = (showActions: boolean, showVariables: boolean, isDuplicate: boolean): SxProps<Theme> => (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: showActions || showVariables ? theme.vars.palette.background.userInputBackground : 'transparent',
  border: `0.0625rem solid ${theme.vars.palette.border.table}`,
  '&:hover': {
    border: showActions || showVariables ? `0.0625rem solid ${theme.vars.palette.border.table}` : `0.0625rem solid ${theme.vars.palette.border.lines}`,
  },
  boxSizing: 'border-box',
  ...(isDuplicate && {
    border: `0.0625rem solid ${theme.vars.palette.border.attention}`,
    backgroundColor: theme.vars.palette.background.attention,
  }),
});

export const cardHeaderSx = (showActions: boolean, showVariables: boolean, hasVariables: boolean): SxProps<Theme> => (theme: Theme) => ({
  borderTopLeftRadius: theme.vars.shape.radiusMd,
  borderTopRightRadius: theme.vars.shape.radiusMd,
  ...(!(showActions || showVariables) && {
    borderBottomLeftRadius: theme.vars.shape.radiusMd,
    borderBottomRightRadius: theme.vars.shape.radiusMd,
  }),
  height: hasVariables ? '4.25rem' : '3.75rem',
  minHeight: hasVariables ? '4.25rem' : '3.75rem',
  boxSizing: 'border-box',
  display: 'flex',
  padding: '0.5rem 1rem',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '1rem',
  backgroundColor: showActions || showVariables ? 'transparent' : theme.vars.palette.background.userInputBackground,
  '&:hover': {
    backgroundColor: showActions || showVariables ? 'transparent' : theme.vars.palette.background.toolCard.hover,
  },
  // The action cluster is revealed on hover OR on focus-within, and it is
  // revealed by OPACITY (see `actionButtonSx`). The baseline toggled
  // `display` on hover alone, which took every action — including the one
  // control that detaches a toolkit from an agent — out of the tab order and
  // out of the accessibility tree, so a keyboard or touch user could not
  // remove a toolkit at all (WCAG 2.1.1, #849). A `:focus-within` rule cannot
  // rescue a control that can never hold focus, so the hiding mechanism itself
  // had to change.
  '&:hover .agents-tool-card-action, &:focus-within .agents-tool-card-action': { opacity: 1 },
});

export const entityIconSx: SxProps<Theme> = { minWidth: '2.125rem', width: '2.125rem', height: '2.125rem' };
export const entityIconImageSx: SxProps<Theme> = (theme: Theme) => ({ width: '2.125rem', height: '2.125rem', borderRadius: theme.vars.shape.radiusPill });

export const contentBoxSx = (isAgentOrPipeline: boolean): SxProps<Theme> => ({
  display: 'flex',
  flexDirection: 'column',
  cursor: 'default',
  flex: '1 1 0',
  minWidth: 0,
  height: isAgentOrPipeline ? 'auto' : '2.75rem',
  minHeight: '2.75rem',
  gap: isAgentOrPipeline ? '0.125rem' : '0rem',
});

export const titleRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem', minWidth: 0 };
export const toolkitNameSx: SxProps<Theme> = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0 };

export const attachmentButtonSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: '1.25rem',
  width: '1.25rem',
  height: '1.25rem',
  borderRadius: theme.vars.shape.radiusPill,
  padding: '0rem',
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  '&:disabled': { color: theme.vars.palette.text.metrics },
});

export const attachIconSx: SxProps<Theme> = { width: '0.75rem', height: '0.75rem' };

export const buttonsContainerSx: SxProps<Theme> = { alignSelf: 'center', marginTop: '0rem', display: 'flex', gap: '0.25rem', alignItems: 'center', flexShrink: 0 };

/**
 * `agents-tool-card-action` class name is what `cardHeaderSx`'s
 * `:hover, :focus-within` rule above reveals — same reveal-on-row-hover
 * behaviour as the baseline's `#RefreshButton`/`#OpenInNewTabButton`/
 * `#DeleteButton`/`#LogoutButton` id-selector rule, ported to a class (ids are
 * not unique across a list of cards).
 *
 * Hidden by `opacity: 0`, NOT by `display: 'none'` (#849). `display: none`
 * removes an element from the tab order and from the accessibility tree, so
 * the detach control was operable by mouse only; the same fix
 * `OpenAPISchemaInput` (`editorWrapperSx`), `BucketList` and `ImageAttachment`
 * each already carry for this class of defect.
 *
 * Two product-visible consequences, both chosen deliberately:
 *  - The cluster now keeps its box at rest, so the card reserves the actions'
 *    width instead of reflowing the toolkit name when a pointer arrives. Same
 *    trade `BucketList`'s own comment records ("no reflow when a row is
 *    hovered").
 *  - On a device with no hover at all (`@media (hover: none)` — a touch
 *    screen), the actions are simply always visible: there is no pointer that
 *    could ever trigger the reveal, and a control that is only reachable by
 *    tabbing to an invisible button is not a usable answer for a touch user.
 */
export const actionButtonSx: SxProps<Theme> = {
  display: 'flex',
  opacity: 0,
  // The reveal is instant on hover today; a transition would make the control
  // look unavailable for the moment a keyboard user has already focused it.
  '&:focus-visible': { opacity: 1 },
  '@media (hover: none)': { opacity: 1 },
};
export const actionIconSx: SxProps<Theme> = { width: '1rem', height: '1rem' };

export const statusIconBoxSx = (online: boolean): SxProps<Theme> => (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  marginLeft: '0.25rem',
  color: online ? theme.vars.palette.icon.fill.default : theme.vars.palette.icon.fill.attention,
});

export const variablesToggleSx: SxProps<Theme> = (theme: Theme) => ({
  marginTop: '0rem',
  marginBottom: '0rem',
  display: 'flex',
  alignItems: 'center',
  gap: '0.25rem',
  minWidth: 0,
  cursor: 'pointer',
  color: theme.vars.palette.text.primary,
  '&:hover': { color: theme.vars.palette.primary.main },
});

export const variablesToggleLabelSx: SxProps<Theme> = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
export const variablesToggleCountSx: SxProps<Theme> = { flexShrink: 0, whiteSpace: 'nowrap' };

export const attentionIconSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  width: '1rem',
  height: '1rem',
  marginTop: '0.125rem',
  fill: theme.vars.palette.icon.fill.attention,
});
