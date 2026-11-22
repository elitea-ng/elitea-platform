import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

export interface NodeCardV2SummaryProps {
  readonly primary: string;
  readonly secondary?: string | undefined;
  readonly isNotConfigured?: boolean | undefined;
}

const rootSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  width: '100%',
  minWidth: 0,
  padding: '0.75rem 1rem 0',
  boxSizing: 'border-box',
};

const notConfiguredSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.warning.main,
});

export const NodeCardV2Summary = memo(function NodeCardV2Summary({
  primary,
  secondary,
  isNotConfigured = false,
}: NodeCardV2SummaryProps): ReactNode {
  return (
    <Box sx={rootSx} data-testid="pipeline-node-v2-summary">
      {isNotConfigured ? (
        <Typography variant="bodySmall" sx={notConfiguredSx} noWrap data-testid="pipeline-node-v2-not-configured">
          {t('pipelines.nodeCardV2.notConfigured', 'Not configured')}
        </Typography>
      ) : (
        <>
          <Typography variant="bodySmall" color="text.secondary" noWrap data-testid="pipeline-node-v2-summary-primary">
            {primary || '—'}
          </Typography>
          {secondary ? (
            <Typography variant="bodySmall" color="text.primary" noWrap data-testid="pipeline-node-v2-summary-secondary">
              {secondary}
            </Typography>
          ) : null}
        </>
      )}
    </Box>
  );
});
