/**
 * Structured React-only summary of settled `node_errors` (`Failed · continued`)
 * for the selected timeline step or collapsed group (5b-1 D12).
 *
 * Every field (`node`, `handled`, `supportRef`, `code`, `message`) is rendered
 * as plain React text — never raw HTML.
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import ErrorOutlineOutlinedIcon from '@mui/icons-material/ErrorOutlineOutlined';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { TimelineContinuedFailure } from '../../lib/run-state-timeline/timeline.types';

import {
  continuedFailureBannerSx,
  continuedFailureRowSx,
  runStatusErrorIconSx,
  runStatusErrorTextSx,
} from './runStateTimeline.styles';

export interface TimeLineContinuedFailureSummaryProps {
  readonly failures: readonly TimelineContinuedFailure[];
}

export const TimeLineContinuedFailureSummary = memo(function TimeLineContinuedFailureSummary({
  failures,
}: TimeLineContinuedFailureSummaryProps): ReactNode {
  if (failures.length === 0) return null;

  return (
    <Box
      component="output"
      sx={continuedFailureBannerSx}
      data-testid="pipeline-run-details-continued-failures"
    >
      {failures.map((failure, index) => (
        <Box
          key={`${failure.node}-${failure.supportRef ?? index}`}
          sx={continuedFailureRowSx}
        >
          <ErrorOutlineOutlinedIcon sx={runStatusErrorIconSx} />
          <Typography
            variant="bodySmall"
            sx={runStatusErrorTextSx}
          >
            {`${failure.node} · ${t('pipelines.flowEditor.state.failedContinued', 'Failed · continued')}`}
          </Typography>
          {failure.supportRef && (
            <Typography
              variant="bodySmall"
              color="text.secondary"
              data-testid={`pipeline-run-details-continued-failure-ref-${index}`}
            >
              {t('pipelines.flowEditor.state.supportRefLabel', 'Support ref: {{ref}}', {
                ref: failure.supportRef,
              })}
            </Typography>
          )}
          {failure.message && (
            <Typography
              variant="bodySmall"
              color="text.secondary"
            >
              {failure.message}
            </Typography>
          )}
        </Box>
      ))}
    </Box>
  );
});
