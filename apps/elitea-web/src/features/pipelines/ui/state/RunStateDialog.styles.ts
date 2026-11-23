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
