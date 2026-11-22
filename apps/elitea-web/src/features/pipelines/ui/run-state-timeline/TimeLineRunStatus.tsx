/**
 * Live run status indicator for the grouped timeline header (`EL-3734` / `#1585`,
 * ported from `EliteaUI` `TimeLineRunStatus.jsx`).
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import ErrorOutlineOutlinedIcon from '@mui/icons-material/ErrorOutlineOutlined';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AttentionIcon } from '@/shared/ui/icons/attention-icon';

import { PipelineStatus } from '../../lib/flow-editor/constants/flowEditor.constants';
import type { GroupedTimelineStep } from '../../lib/run-state-timeline/timeline.types';

import {
  runStatusAttentionBoxSx,
  runStatusErrorIconSx,
  runStatusErrorTextSx,
  runStatusIconBoxSx,
  runStatusInactiveTextSx,
  runStatusPublishedTextSx,
  runStatusRowSx,
} from './runStateTimeline.styles';

export interface TimeLineRunStatusProps {
  readonly lastStep: GroupedTimelineStep | undefined;
}

interface StatusPresentation {
  readonly label: string;
  readonly textSx: SxProps<Theme>;
  readonly icon?: ReactNode;
}

function getStatusPresentation(status: string | undefined): StatusPresentation | undefined {
  if (!status) return undefined;
  switch (status) {
    case PipelineStatus.InProgress:
      return {
        label: t('pipelines.flowEditor.state.performing', 'Performing'),
        textSx: runStatusInactiveTextSx,
        icon: (
          <Box sx={runStatusIconBoxSx}>
            <CircularProgress
              size={14}
              sx={runStatusInactiveTextSx}
            />
          </Box>
        ),
      };
    case PipelineStatus.Error:
      return {
        label: t('pipelines.flowEditor.state.error', 'Error'),
        textSx: runStatusErrorTextSx,
        icon: (
          <Box sx={runStatusIconBoxSx}>
            <ErrorOutlineOutlinedIcon sx={runStatusErrorIconSx} />
          </Box>
        ),
      };
    case PipelineStatus.Interrupt:
      return {
        label: t('pipelines.flowEditor.state.userActionWaiting', 'User action waiting...'),
        textSx: runStatusInactiveTextSx,
        icon: (
          <Box sx={runStatusAttentionBoxSx}>
            <AttentionIcon
              width="14"
              height="14"
            />
          </Box>
        ),
      };
    case PipelineStatus.Completed:
      return {
        label: t('pipelines.flowEditor.state.completed', 'Completed'),
        textSx: runStatusPublishedTextSx,
      };
    case PipelineStatus.Stopped:
      return {
        label: t('pipelines.flowEditor.state.stopped', 'Stopped'),
        textSx: runStatusInactiveTextSx,
        icon: (
          <Box sx={runStatusAttentionBoxSx}>
            <AttentionIcon
              width="14"
              height="14"
            />
          </Box>
        ),
      };
    default:
      return undefined;
  }
}

export const TimeLineRunStatus = memo(function TimeLineRunStatus({ lastStep }: TimeLineRunStatusProps): ReactNode {
  const indicator = getStatusPresentation(lastStep?.status);

  return (
    <Box sx={runStatusRowSx}>
      <Typography
        variant="bodyMedium"
        color="text.secondary"
      >
        {`${lastStep?.id ?? ''}:`}
      </Typography>
      {indicator && (
        <>
          {indicator.icon}
          <Typography
            variant="bodyMedium"
            sx={indicator.textSx}
          >
            {indicator.label}
          </Typography>
        </>
      )}
    </Box>
  );
});
