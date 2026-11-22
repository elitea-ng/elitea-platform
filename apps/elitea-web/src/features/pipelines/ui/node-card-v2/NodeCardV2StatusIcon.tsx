import type { ReactNode } from 'react';
import { memo } from 'react';

import ErrorOutlineRoundedIcon from '@mui/icons-material/ErrorOutlineRounded';
import HighlightOffOutlinedIcon from '@mui/icons-material/HighlightOffOutlined';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { NodeCardV2Status } from './nodeCardV2.helpers';

export interface NodeCardV2StatusIconProps {
  readonly status: NodeCardV2Status;
  readonly issues?: readonly string[] | undefined;
}

const rootSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  flexShrink: 0,
  width: '1.5rem',
  height: '1.5rem',
};

const tooltipLineSx: SxProps<Theme> = { display: 'block' };

const spinnerSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.primary.main,
});

const errorIconSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.error.main,
});

const warningIconSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.warning.main,
});

function normalizeTestIdStatus(status: NonNullable<NodeCardV2Status>): string {
  return status === 'needsConfiguration' ? 'needs-configuration' : status;
}

export const NodeCardV2StatusIcon = memo(function NodeCardV2StatusIcon({
  status,
  issues = [],
}: NodeCardV2StatusIconProps): ReactNode {
  if (!status) return null;

  const statusTestId = `pipeline-node-v2-status-${normalizeTestIdStatus(status)}`;

  if (status === 'needsConfiguration') {
    const tooltipContent = issues.length > 0 ? (
      <Box component="span">
        {issues.map((issue) => (
          <Box key={issue} component="span" sx={tooltipLineSx}>
            {issue}
          </Box>
        ))}
      </Box>
    ) : (
      t('pipelines.nodeCardV2.needsConfiguration', 'Node needs configuration')
    );

    return (
      <Tooltip placement="top" title={tooltipContent}>
        <Box sx={rootSx} data-testid={statusTestId} aria-label={t('pipelines.nodeCardV2.needsConfiguration', 'Node needs configuration')}>
          <ErrorOutlineRoundedIcon fontSize="small" sx={warningIconSx} />
        </Box>
      </Tooltip>
    );
  }

  return (
    <Box sx={rootSx} data-testid={statusTestId}>
      {status === 'running' ? (
        <CircularProgress size="1rem" thickness={5} sx={spinnerSx} />
      ) : (
        <HighlightOffOutlinedIcon fontSize="small" sx={errorIconSx} />
      )}
    </Box>
  );
});
