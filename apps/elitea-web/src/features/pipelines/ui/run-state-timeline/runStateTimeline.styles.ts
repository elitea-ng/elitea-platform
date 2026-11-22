/**
 * Theme-token style factories for the grouped RunStateDialog timeline
 * (`EL-3734` / `#1585`). Uses `theme.vars.palette.*`, `theme.vars.shape.*`,
 * and `theme.spacing(...)` with zero `!important` overrides or ad-hoc literals.
 */
import type { SxProps, Theme } from '@mui/material/styles';
import { stepClasses } from '@mui/material/Step';
import { stepConnectorClasses } from '@mui/material/StepConnector';

import {
  GROUP_TOGGLE_BUTTON_HEIGHT,
  STEP_CONNECTOR_MIN_WIDTH,
  STEP_MIN_WIDTH,
  STEP_NAME_EDGE_MAX_WIDTH,
  STEP_NAME_MAX_WIDTH,
} from '../../lib/run-state-timeline/timeline.constants';

const CIRCLE_RADIUS = '50%';

export const timelineHeaderSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  width: '100%',
  minHeight: '2.25rem',
  justifyContent: 'space-between',
  alignItems: 'center',
  padding: `${theme.spacing(1)} ${theme.spacing(3)} 0 ${theme.spacing(3)}`,
  boxSizing: 'border-box',
});

export const timelineCurrentStepSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  gap: theme.spacing(0.5),
  alignItems: 'center',
  minWidth: 0,
});

export const timelineHeaderActionsSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(2),
  flexShrink: 0,
});

export const expandAllContainerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  height: '1.75rem',
  whiteSpace: 'nowrap',
});

export const groupToggleBtnSx: SxProps<Theme> = {
  flexShrink: 0,
  backgroundColor: 'transparent',
  zIndex: 2,
  minWidth: '1.75rem',
  height: GROUP_TOGGLE_BUTTON_HEIGHT,
  padding: 0,
};

export const runStatusRowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  gap: theme.spacing(0.5),
  alignItems: 'center',
  justifyContent: 'flex-end',
});

export const runStatusIconBoxSx: SxProps<Theme> = {
  width: '0.875rem',
  height: '0.875rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
};

export const runStatusAttentionBoxSx: SxProps<Theme> = (theme: Theme) => ({
  width: '0.875rem',
  height: '0.875rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  color: theme.vars.palette.status.onModeration,
});

export const runStatusInactiveTextSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.icon.fill.inactive,
});

export const runStatusErrorTextSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.status.rejected,
});

export const runStatusErrorIconSx: SxProps<Theme> = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  color: theme.vars.palette.status.rejected,
});

export const runStatusPublishedTextSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.status.published,
});

export const timelineScrollSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  width: '100%',
  overflowX: 'auto',
  overflowY: 'hidden',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.flowNode}`,
});

export const timelineStepperSx: SxProps<Theme> = (theme: Theme) => ({
  boxSizing: 'border-box',
  width: 'max-content',
  minWidth: '100%',
  padding: `${theme.spacing(4)} ${theme.spacing(3)} ${theme.spacing(5.25)}`,
  [`& > .${stepConnectorClasses.root}`]: {
    minWidth: STEP_CONNECTOR_MIN_WIDTH,
    [`& .${stepConnectorClasses.line}`]: {
      marginInline: `calc(${STEP_MIN_WIDTH} / -2)`,
    },
    [`&:nth-of-type(2) .${stepConnectorClasses.line}`]: {
      marginInlineStart: `calc(${STEP_MIN_WIDTH} / -4)`,
    },
    [`&:nth-last-of-type(2) .${stepConnectorClasses.line}`]: {
      marginInlineEnd: `calc(${STEP_MIN_WIDTH} / -4)`,
    },
  },
});

export function timelineConnectorSx(isError: boolean): SxProps<Theme> {
  return (theme: Theme) => {
    const lineColor = isError ? theme.vars.palette.status.rejected : theme.vars.palette.status.published;
    return {
      [`&.${stepConnectorClasses.active}`]: {
        [`& .${stepConnectorClasses.line}`]: { borderColor: lineColor },
      },
      [`&.${stepConnectorClasses.completed}`]: {
        [`& .${stepConnectorClasses.line}`]: { borderColor: lineColor },
      },
      [`& .${stepConnectorClasses.line}`]: {
        borderColor: lineColor,
        borderTopWidth: '0.375rem',
        borderRadius: theme.vars.shape.radiusMd,
        zIndex: 0,
      },
    };
  };
}

export const timelineStepSx: SxProps<Theme> = {
  minWidth: STEP_MIN_WIDTH,
  height: '1.5rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  padding: 0,
  position: 'relative',
  '&:first-of-type, &:last-of-type': {
    minWidth: `calc(${STEP_MIN_WIDTH} / 2)`,
  },
};

export const stepNameContainerSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'absolute',
  bottom: 'calc(100% + 0.125rem)',
  left: '50%',
  transform: 'translateX(-50%)',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  gap: theme.spacing(0.5),
  minHeight: GROUP_TOGGLE_BUTTON_HEIGHT,
  width: 'max-content',
  maxWidth: STEP_NAME_MAX_WIDTH,
  [`.${stepClasses.root}:first-of-type > &, .${stepClasses.root}:last-of-type > &`]: {
    maxWidth: STEP_NAME_EDGE_MAX_WIDTH,
  },
});

export const stepNameWithCountSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'flex-start',
  gap: theme.spacing(0.25),
  minWidth: 0,
});

export const stepNameTextSx: SxProps<Theme> = {
  width: 'auto',
  minWidth: 0,
  marginRight: 0,
  textAlign: 'center',
};

export const stepCountBadgeWrapperSx: SxProps<Theme> = {
  display: 'flex',
  flexShrink: 0,
  marginTop: '-0.375rem',
};

export const stepCountBadgeSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  minWidth: '1rem',
  height: '1rem',
  padding: `0 ${theme.spacing(0.5)}`,
  borderRadius: theme.vars.shape.radiusLg,
  backgroundColor: theme.vars.palette.background.eliteaDefault,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  color: theme.vars.palette.text.secondary,
  fontSize: theme.typography.labelSmall.fontSize,
  lineHeight: 1,
  fontWeight: 600,
});

export function stepDotOuterSx(active: boolean, isError: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    width: '1.3125rem',
    height: '1.3125rem',
    borderRadius: CIRCLE_RADIUS,
    display: 'flex',
    justifyContent: 'center',
    alignItems: 'center',
    boxSizing: 'border-box',
    padding: 0,
    background: 'transparent',
    font: 'inherit',
    cursor: 'pointer',
    border: active
      ? `0.0625rem solid ${isError ? theme.vars.palette.status.rejected : theme.vars.palette.status.published}`
      : 0,
    zIndex: 1,
    '&:hover': {
      width: '1.5rem',
      height: '1.5rem',
    },
  });
}

export function stepDotInnerSx(isError: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    width: '1.25rem',
    height: '1.25rem',
    borderRadius: CIRCLE_RADIUS,
    boxSizing: 'border-box',
    backgroundColor: isError ? theme.vars.palette.status.rejected : theme.vars.palette.status.published,
    border: `0.1875rem solid ${theme.vars.palette.background.tabPanel}`,
    zIndex: 1,
    '&:hover': {
      width: '1.4375rem',
      height: '1.4375rem',
    },
  });
}

export const stepTimestampContainerSx: SxProps<Theme> = {
  position: 'absolute',
  top: 'calc(100% + 0.125rem)',
  left: '50%',
  transform: 'translateX(-50%)',
  display: 'flex',
  justifyContent: 'center',
  width: 'max-content',
  whiteSpace: 'nowrap',
};

export const stepTimestampRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  minHeight: '1.25rem',
};

export const continuedFailureBannerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.75),
  width: '100%',
  padding: `${theme.spacing(1)} ${theme.spacing(3)}`,
  boxSizing: 'border-box',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.flowNode}`,
  backgroundColor: theme.vars.palette.background.eliteaDefault,
});

export const continuedFailureRowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  flexWrap: 'wrap',
  gap: theme.spacing(1),
});
