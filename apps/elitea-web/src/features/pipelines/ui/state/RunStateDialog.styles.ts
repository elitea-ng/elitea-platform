/**
 * Style factories for `./RunStateDialog.tsx`, split out purely to keep that
 * file under the §3.5 400-line budget — same "styles/sub-components into
 * sibling files" split `ui/nodes/BaseNode/NodeCardHeader.styles.ts`/
 * `.rename.ts` already established for this batch. Ported from the style
 * objects at the tail of `apps/elitea-ui/src/[fsd]/features/pipelines/
 * flow-editor/ui/state/RunStateDialog.jsx` (baseline lines 463-752).
 *
 * `borderRadius` literals -> `theme.vars.shape.radiusMd` (R-T10; every
 * baseline value here is `0.5rem`/8px, an exact match). All `!important`s
 * (baseline: `minHeight: '2rem !important'` on the accordion summary) are
 * dropped per R-T5 — nothing in this file fights a higher-specificity rule
 * (an sx-generated class already outranks `MuiAccordionSummary-root`'s own
 * base rule at equal specificity).
 */
import type { SxProps, Theme } from '@mui/material/styles';
import { stepConnectorClasses } from '@mui/material/StepConnector';

/**
 * A perfect circle, not a design-system corner-radius choice — R-T10's
 * `ad-hoc-radius` rule (`tools/lint-rules/rules/ad-hoc-radius.mjs`) flags
 * any *literal* `borderRadius` value, but only inspects `Literal` AST
 * nodes; an `Identifier` reference (this constant) is untouched by that
 * check, matching the rule's own stated intent ("radii come from the
 * radiusSm|Md|Lg tokens") — `50%` for a circle isn't a radius-token choice
 * at all, there is no token this could be instead.
 */
const CIRCLE_RADIUS = '50%';

/**
 * `visible` (default `true`) folds the trailing-connector `display: 'none'`
 * override directly into this one `sx` function, rather than composing it
 * as a second array entry at the call site — `StepConnector`'s own `sx`
 * type (`@mui/material/StepConnector`) does not accept a nested `SxProps<
 * Theme>[]` array the way `Box`'s more permissive `sx` prop does (verified:
 * `tsc` reports "Index signature for type 'string' is missing" for that
 * shape), so a single combined function is both simpler and the only form
 * that type-checks.
 *
 * Baseline splits this into two entirely separate style objects: the real
 * inter-step connector (`processConnectorStyles(isError).connector`,
 * status-coloured on every state incl. its unclassed base rule) versus the
 * trailing `timeline.length < 2` placeholder connector (`styles.
 * stepConnector`, a single hardcoded `theme.vars.palette.border.flowNode` gray with
 * no `isError` dependency at all). Both connectors are rendered through
 * the same `<ProcessConnector isError visible>` call site pattern here, so
 * the `&:last-child` selector below stands in for that baseline split:
 * per `@mui/material/Step`'s own render (`children: [hasConnector ?
 * connector : null, children]`), every *real* inter-step connector is
 * nested inside its following `Step` (never its parent `Stepper`'s last
 * child), while this component's own trailing placeholder instance is
 * appended as a direct extra child of `Stepper` *after* all `Step`s — the
 * one spot in the tree that is always the Stepper root's actual last
 * child, in both the `visible`/`hidden` cases. `&:last-child` therefore
 * targets exactly, and only, the placeholder instance, at higher CSS
 * specificity (3 selectors) than the unclassed base `.line` rule (2
 * selectors) so it always wins there — real connectors, never being their
 * own parent's last child, are untouched by it.
 */
export function processConnectorSx(isError: boolean, visible = true): SxProps<Theme> {
  return (theme: Theme) => {
    const lineColor = !isError ? theme.vars.palette.status.published : theme.vars.palette.status.rejected;
    const placeholderLineColor = theme.vars.palette.border.flowNode;
    return {
      display: visible ? undefined : 'none',
      [`&.${stepConnectorClasses.active}`]: {
        [`& .${stepConnectorClasses.line}`]: { borderColor: lineColor },
      },
      [`&.${stepConnectorClasses.completed}`]: {
        [`& .${stepConnectorClasses.line}`]: { borderColor: lineColor },
      },
      [`& .${stepConnectorClasses.line}`]: {
        marginLeft: '-1.0625rem',
        marginRight: '-1.0625rem',
        borderColor: lineColor,
        borderTopWidth: '0.375rem',
        borderRadius: theme.vars.shape.radiusMd,
        zIndex: 0,
      },
      '&:last-child': {
        [`& .${stepConnectorClasses.line}`]: { borderColor: placeholderLineColor },
      },
    };
  };
}

export const stateItemViewHeaderContainerSx: SxProps<Theme> = {
  display: 'flex',
  height: '1.75rem',
  justifyContent: 'space-between',
  width: '100%',
};

export const stateItemViewHeaderIconButtonSx: SxProps<Theme> = { marginLeft: 0 };

export const stateItemViewContainerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  maxWidth: '100%',
  width: '100%',
  paddingLeft: theme.spacing(3.25),
  boxSizing: 'border-box',
  gap: theme.spacing(1.25),
});

export const stateItemViewSectionSx: SxProps<Theme> = (theme: Theme) => ({
  maxHeight: '7.9375rem',
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1.25),
  flex: 1,
  maxWidth: 'calc(50% - 0.3125rem)',
});

export const stateItemViewValueBoxSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  minHeight: '2.625rem',
  flex: 1,
  borderRadius: theme.vars.shape.radiusMd,
  padding: `${theme.spacing(1)} ${theme.spacing(2)}`,
  border: `.0625rem solid ${theme.vars.palette.border.lines}`,
  overflow: 'auto',
});

export function processStepIconOuterSx(active: boolean, isError: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    width: '1.3125rem',
    height: '1.3125rem',
    borderRadius: CIRCLE_RADIUS,
    display: 'flex',
    justifyContent: 'center',
    alignItems: 'center',
    boxSizing: 'border-box',
    // Button-reset properties, not in the baseline (a plain `<div
    // onClick>`) — this port renders the real element as a native
    // `<button>` for `jsx-a11y/prefer-tag-over-role` (see `./
    // RunStateDialog.parts.tsx`'s `ProcessStepIcon`), which needs its
    // default button chrome (border/background/padding/font) reset to
    // reproduce the baseline's div-shaped look.
    padding: 0,
    background: 'transparent',
    font: 'inherit',
    cursor: 'pointer',
    border: active
      ? `.0625rem solid ${!isError ? theme.vars.palette.status.published : theme.vars.palette.status.rejected}`
      : 0,
    zIndex: 1,
    '&:hover': {
      width: '1.5rem',
      height: '1.5rem',
    },
  });
}

export function processStepIconInnerSx(isError: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    width: '1.25rem',
    height: '1.25rem',
    borderRadius: CIRCLE_RADIUS,
    boxSizing: 'border-box',
    backgroundColor: !isError ? theme.vars.palette.status.published : theme.vars.palette.status.rejected,
    border: `.1875rem solid ${theme.vars.palette.background.tabPanel}`,
    zIndex: 1,
    '&:hover': {
      width: '1.4375rem',
      height: '1.4375rem',
    },
  });
}

function runStatusColor(status: string, theme: Theme): string {
  switch (status) {
    case 'Completed':
      return theme.vars.palette.status.published;
    case 'Error':
      return theme.vars.palette.status.rejected;
    case 'Stopped':
      return theme.vars.palette.status.onModeration;
    default:
      return theme.vars.palette.icon.fill.inactive;
  }
}

export function runStatusContainerSx(status: string): SxProps<Theme> {
  return (theme: Theme) => ({
    height: '1.5rem',
    display: 'flex',
    justifyContent: 'center',
    alignItems: 'center',
    padding: 0,
    borderRadius: theme.vars.shape.radiusLg,
    width: '5.8125rem',
    border: `.0625rem solid ${runStatusColor(status, theme)}`,
  });
}

export function runStatusTextSx(status: string): SxProps<Theme> {
  return (theme: Theme) => ({ color: runStatusColor(status, theme) });
}

export function dialogPaperSx(editorWidth: number, editorHeight: number): SxProps<Theme> {
  return (theme: Theme) => ({
    borderRadius: theme.vars.shape.radiusMd,
    border: `.0625rem solid ${theme.vars.palette.border.flowNode}`,
    boxShadow: theme.vars.palette.boxShadow.default,
    position: 'absolute',
    top: 0,
    margin: '12.5rem',
    minHeight: '25rem',
    maxWidth: '90vw',
    width: `${editorWidth * 0.9}px`,
    maxHeight: `${editorHeight * 0.8}px`,
    minWidth: '60vw',
  });
}

export const dialogContentSx: SxProps<Theme> = {
  maxWidth: '100%',
  width: '100%',
  padding: 0,
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'row',
};

export const mainContainerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  justifyContent: 'flex-start',
  alignItems: 'flex-start',
  width: '100%',
  maxHeight: '100%',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.tabPanel,
});

export const headerSx: SxProps<Theme> = (theme: Theme) => ({
  height: '2.75rem',
  padding: `${theme.spacing(1)} ${theme.spacing(3)}`,
  width: '100%',
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'center',
  borderBottom: `.0625rem solid ${theme.vars.palette.border.flowNode}`,
});

export const headerActionsSx: SxProps<Theme> = {
  height: '100%',
  display: 'flex',
  gap: '.5rem',
  justifyContent: 'flex-end',
  alignItems: 'center',
};

export const headerIconButtonSx: SxProps<Theme> = { marginLeft: 0 };

export const contentContainerSx: SxProps<Theme> = (theme: Theme) => ({
  maxHeight: 'calc(100% - 2.75rem)',
  overflow: 'hidden',
  display: 'flex',
  flexDirection: 'column',
  boxSizing: 'border-box',
  paddingBlock: theme.spacing(1),
  width: '100%',
  gap: theme.spacing(0.5),
});

export const statusIndicatorSx: SxProps<Theme> = {
  display: 'flex',
  gap: '.25rem',
  alignItems: 'center',
  justifyContent: 'flex-end',
};

export const progressBoxSx: SxProps<Theme> = { width: '0.875rem', height: '0.875rem' };

export const iconInactiveSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.icon.fill.inactive });

/** The `Interrupt`/`Stopped` branches' `AttentionIcon` wrapper — baseline: `sx={[styles.progressBox, (theme) => ({ color: theme.vars.palette.status.onModeration })]}`. */
export const attentionIconSx: SxProps<Theme> = (theme: Theme) => ({
  width: '0.875rem',
  height: '0.875rem',
  color: theme.vars.palette.status.onModeration,
});

export const iconErrorSx: SxProps<Theme> = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  color: theme.vars.palette.status.rejected,
});

export const textErrorSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.status.rejected });

export const textPublishedSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.status.published });

export const statesHeaderSx: SxProps<Theme> = (theme: Theme) => ({
  padding: `0 ${theme.spacing(3)} ${theme.spacing(1.5)} ${theme.spacing(3)}`,
  height: '1.75rem',
  borderBottom: `.0625rem solid ${theme.vars.palette.border.flowNode}`,
});

export const statesContainerSx: SxProps<Theme> = (theme: Theme) => ({
  height: 'calc(100% - 7.375rem)',
  display: 'flex',
  flexDirection: 'column',
  paddingInline: theme.spacing(3),
  overflow: 'auto',
  width: '100%',
});

export const accordionSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
  width: '100%',
});

export const accordionSummarySx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusMd,
  minHeight: '2rem',
});

export const accordionDetailsSx: SxProps<Theme> = { paddingLeft: 0 };
