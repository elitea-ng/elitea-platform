/**
 * After an import (issue 3137): what landed, and what is left to do — the
 * toolkits to connect, the memory seeds to review, the heartbeat to turn on —
 * and the model each agent runs on, with its context window (issue 3143). An
 * agent whose file named no model this install holds got the project's
 * default model; that is said as a warning, never left for the person to find
 * out from a smaller context than they expected.
 * Shown by Import and by the blueprint gallery's Import alike.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { ImportedAgentModelNotice, ImportFollowUps } from '../lib/importFollowUps';

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

function modelText(notice: ImportedAgentModelNotice): string {
  const model =
    notice.contextWindow === undefined
      ? notice.model
      : t('features.agentLifecycle.import.modelWithWindow', '{{model}} ({{window}}-token context)', {
          model: notice.model,
          window: new Intl.NumberFormat().format(notice.contextWindow),
        });
  if (!notice.defaulted) return t('features.agentLifecycle.import.modelKept', '{{agent}} runs on {{model}}.', { agent: notice.agent, model });
  if (notice.requested === '') {
    return t('features.agentLifecycle.import.modelDefaulted', '{{agent}}: the file names no model, so it runs on this project\'s default, {{model}}. Change it in the agent\'s settings.', {
      agent: notice.agent,
      model,
    });
  }
  return t(
    'features.agentLifecycle.import.modelReplaced',
    '{{agent}}: {{requested}} is not available here, so it runs on this project\'s default, {{model}}. Change it in the agent\'s settings.',
    { agent: notice.agent, requested: notice.requested, model },
  );
}

function ImportedModels({ models, testIdPrefix }: { readonly models: ImportFollowUps['models']; readonly testIdPrefix: string }): ReactNode {
  if (models.length === 0) return null;
  const defaulted = models.filter((notice) => notice.defaulted);
  const kept = models.filter((notice) => !notice.defaulted);
  return (
    <>
      {defaulted.length > 0 && (
        <Alert severity="warning" data-testid={`${testIdPrefix}-import-model-defaulted`}>
          <Box component="ul" sx={listSx}>
            {defaulted.map((notice) => (
              <Typography key={notice.agent} component="li" variant="bodySmall">
                {modelText(notice)}
              </Typography>
            ))}
          </Box>
        </Alert>
      )}
      {kept.length > 0 && (
        <Box component="ul" sx={listSx} data-testid={`${testIdPrefix}-import-models`}>
          {kept.map((notice) => (
            <Typography key={notice.agent} component="li" variant="bodySmall">
              {modelText(notice)}
            </Typography>
          ))}
        </Box>
      )}
    </>
  );
}

export function ImportFollowUpsView({ names, followUps, testIdPrefix }: ImportFollowUpsViewProps): ReactNode {
  return (
    <Box sx={rootSx} data-testid={`${testIdPrefix}-import-done`}>
      <Alert severity="success">
        {t('features.agentLifecycle.import.done', 'Imported: {{names}}', { names: names.join(', ') })}
      </Alert>
      <ImportedModels models={followUps.models} testIdPrefix={testIdPrefix} />
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
