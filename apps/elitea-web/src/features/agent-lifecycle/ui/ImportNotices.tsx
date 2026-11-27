/**
 * What the person should know before an import, as one callout (issue
 * 3142): the toolkits that land without credentials, the heartbeat to turn
 * on after publishing, the agents that name no model (they get the project's
 * default, issue 3143), and what the file cannot carry. Nothing to say draws
 * nothing.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { ImportDocumentSummary } from '../lib/importSummary';

function notices(summary: ImportDocumentSummary): string[] {
  const lines: string[] = [];
  if (summary.toolkitCount > 0) {
    lines.push(
      t('features.agentLifecycle.import.noticeToolkits', '{{count}} toolkits import without credentials. Connect them after the import.', {
        count: summary.toolkitCount,
      }),
    );
  }
  if (summary.heartbeatAgents.length > 0) {
    lines.push(
      t('features.agentLifecycle.import.noticeHeartbeat', 'Heartbeat is configured for {{names}}. Turn it on in the Heartbeat tab after publishing.', {
        names: summary.heartbeatAgents.join(', '),
      }),
    );
  }
  const unnamed = summary.agents.filter((agent) => agent.model === undefined).map((agent) => agent.name);
  if (unnamed.length > 0) {
    lines.push(
      t('features.agentLifecycle.import.noticeNoModel', '{{names}}: the file names no model, so the import gives them this project\'s default model and says which.', {
        names: unnamed.join(', '),
      }),
    );
  }
  return lines;
}

export function ImportNotices({ summary, testIdPrefix }: { readonly summary: ImportDocumentSummary; readonly testIdPrefix: string }): ReactNode {
  const lines = notices(summary);
  if (lines.length === 0 && summary.skipped.length === 0) return null;
  return (
    <Alert severity="info" role="note" data-testid={`${testIdPrefix}-import-notices`}>
      <AlertTitle>{t('features.agentLifecycle.import.noticesTitle', 'Before you import')}</AlertTitle>
      {lines.length > 0 && (
        <Box component="ul" sx={listSx}>
          {lines.map((line) => (
            <Typography key={line} component="li" variant="bodySmall">
              {line}
            </Typography>
          ))}
        </Box>
      )}
      {summary.skipped.length > 0 && (
        <Box data-testid={`${testIdPrefix}-import-skipped`}>
          <Typography variant="labelSmall" component="div">
            {t('features.agentLifecycle.import.skipped', 'Not imported (this file cannot carry them)')}:
          </Typography>
          <Box component="ul" sx={listSx}>
            {summary.skipped.map((item) => (
              <Typography key={item} component="li" variant="bodySmall">
                {item}
              </Typography>
            ))}
          </Box>
        </Box>
      )}
    </Alert>
  );
}

const listSx: SxProps<Theme> = (theme: Theme) => ({ margin: 0, paddingLeft: theme.spacing(2.5), display: 'flex', flexDirection: 'column', gap: theme.spacing(0.5) });
