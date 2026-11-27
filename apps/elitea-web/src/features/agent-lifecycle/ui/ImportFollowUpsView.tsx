/**
 * After an import (issue 3137): what landed, and what is left to do — the
 * toolkits to connect, the memory seeds to review, the heartbeat to turn on.
 * Shown by Import and by the blueprint gallery's Import alike.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { ImportFollowUps } from '../lib/importFollowUps';

export interface ImportFollowUpsViewProps {
  readonly names: readonly string[];
  readonly followUps: ImportFollowUps;
  readonly testIdPrefix: string;
}

function ToolkitsToConnect({ toolkits, testIdPrefix }: { readonly toolkits: ImportFollowUps['toolkits']; readonly testIdPrefix: string }): ReactNode {
  if (toolkits.length === 0) return null;
  return (
    <Box data-testid={`${testIdPrefix}-import-connect`}>
      <Typography variant="labelMedium">
        {t('features.agentLifecycle.import.connectTitle', 'Connect these toolkits: they were imported without credentials.')}
      </Typography>
      <Box component="ul" sx={listSx}>
        {toolkits.map((toolkit, index) => (
          <Typography key={`${toolkit.name}-${String(index)}`} component="li" variant="bodySmall">
            {toolkit.type === '' || toolkit.type === toolkit.name ? toolkit.name : `${toolkit.name} (${toolkit.type})`}
          </Typography>
        ))}
      </Box>
    </Box>
  );
}

export function ImportFollowUpsView({ names, followUps, testIdPrefix }: ImportFollowUpsViewProps): ReactNode {
  return (
    <Box sx={rootSx} data-testid={`${testIdPrefix}-import-done`}>
      <Alert severity="success">
        {t('features.agentLifecycle.import.done', 'Imported: {{names}}', { names: names.join(', ') })}
      </Alert>
      <ToolkitsToConnect toolkits={followUps.toolkits} testIdPrefix={testIdPrefix} />
      {followUps.memorySeeds > 0 && (
        <Typography variant="bodySmall" data-testid={`${testIdPrefix}-import-seeds`}>
          {t('features.agentLifecycle.import.memorySeeds', '{{count}} memory seeds wait for your review in the Memory tab.', {
            count: followUps.memorySeeds,
          })}
        </Typography>
      )}
      {followUps.heartbeatAgents.map((name) => (
        <Typography key={name} variant="bodySmall" data-testid={`${testIdPrefix}-import-heartbeat`}>
          {t('features.agentLifecycle.import.heartbeat', '{{name}}: Heartbeat is configured; turn it on in the Heartbeat tab after publishing.', {
            name,
          })}
        </Typography>
      ))}
    </Box>
  );
}

const rootSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(1) });
const listSx: SxProps<Theme> = (theme: Theme) => ({ margin: 0, paddingLeft: theme.spacing(2.5) });
