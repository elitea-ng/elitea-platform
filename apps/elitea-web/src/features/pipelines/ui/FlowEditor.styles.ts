/**
 * `FlowEditor.jsx`'s bottom-of-file `flowEditorStyles`/`StyledControls`
 * (baseline lines 644-723), split into its own file purely to keep
 * `FlowEditor.tsx` itself under the §3.5 400-line file-length budget —
 * same reason `ToolCard.styles.ts` split off from `ToolCard.tsx`.
 *
 * DEVIATIONS (all R-T-forced, not stylistic choices — same treatment as
 * `ToolCard.styles.ts`'s own header):
 *  - `theme.palette.*` -> `theme.vars.palette.*` throughout (R-T7).
 *  - The baseline's `stateDrawerButton`'s two `!important`s (beating
 *    `MuiButton`'s `styleOverrides`) are dropped, not waived — R-T5 bans
 *    `!important` in `sx`/`styled` outright, and MUI's `sx` prop already
 *    wins over `styleOverrides` at the CSS layer (same verified claim
 *    `ToolCard.styles.ts`'s own header makes for the identical pattern).
 *  - `StyledControls`'s two `!important`s (baseline lines 700,710 — beating
 *    `@xyflow/react/dist/style.css`'s own `.react-flow__controls-button
 *    svg` base rule, a DIFFERENT library's stylesheet, not a `styleOverrides`
 *    this app owns) are also dropped, same R-T5 fence, no waiver id on
 *    hand. Left as a disclosed, not silently accepted, visual-fidelity risk
 *    — `& .react-flow__controls-button svg` is a class+element selector
 *    pair that is MORE specific than xyflow's own bare-element rule, so it
 *    should still win on specificity even without `!important`, but this
 *    was not verified in a browser.
 */
import { Controls } from '@xyflow/react';

import { styled, type SxProps, type Theme } from '@mui/material/styles';

export const flowEditorContainerSx: SxProps<Theme> = {
  height: '100%',
  position: 'relative',
  width: '100%',
};

export const flowEditorStateBarSx: SxProps<Theme> = {
  position: 'absolute',
  top: '1.25rem',
  left: '1.25rem',
  zIndex: 100,
  display: 'flex',
  gap: '0.75rem',
  width: 'calc(100% - 2.5rem)',
  minWidth: '25rem',
  overflowX: 'scroll',
  pointerEvents: 'none', // Allow events to pass through the container.
  '& > *': {
    pointerEvents: 'auto', // Re-enable events for child components.
  },
};

/** `<ReactFlow defaultViewport>` — a plain coordinate object, not `sx`. */
export const FLOW_EDITOR_DEFAULT_VIEWPORT = { x: 0, y: 0, zoom: 0.75 } as const;

export const flowEditorIconScaleSx: SxProps<Theme> = {
  transform: 'scale(0.8)',
};

export const flowEditorStateDrawerButtonSx: SxProps<Theme> = (theme: Theme) => ({
  height: theme.spacing(4.5),
  padding: theme.spacing(0.75, 1.5),
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.tabPanel,
  color: theme.vars.palette.text.secondary,
  gap: theme.spacing(0.75),
  fontSize: theme.typography.bodyMedium.fontSize,
  fontWeight: 400,
  '&:hover': {
    background: theme.vars.palette.background.dataGrid.main,
    border: `0.0625rem solid ${theme.vars.palette.border.flowNode}`,
  },
});

/** `FlowEditor.jsx:695-722`'s `StyledControls` verbatim, `theme.palette` swapped for `theme.vars.palette` (R-T7). */
export const StyledFlowControls = styled(Controls)(({ theme }) => ({
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusSm,

  '& .react-flow__controls-button svg': {
    color: theme.vars.palette.border.hover,
  },

  '& .react-flow__controls-button': {
    backgroundColor: theme.vars.palette.background.paper,
    borderBottom: `0.0625rem solid ${theme.vars.palette.divider}`,
    '&:hover': {
      backgroundColor: theme.vars.palette.border.table,
      '& svg': {
        color: theme.vars.palette.icon.fill.secondary,
      },
    },
  },

  // Longhand corner properties, not a `'<tl> <tr> <br> <bl>'` shorthand
  // string — the shorthand baseline literal cannot be reproduced by
  // interpolating a `theme.vars.shape.*` token into a template string (its
  // runtime shape is a CSS-variable-backed value under `cssVariables` mode,
  // not necessarily a bare number to unit-suffix), same constraint
  // `ToolCard.styles.ts`'s own header documents for the identical problem.
  '& .react-flow__controls-button:first-of-type': {
    borderTopLeftRadius: theme.vars.shape.radiusSm,
    borderTopRightRadius: theme.vars.shape.radiusSm,
  },

  '& .react-flow__controls-button:last-child': {
    borderBottomLeftRadius: theme.vars.shape.radiusSm,
    borderBottomRightRadius: theme.vars.shape.radiusSm,
  },
}));
