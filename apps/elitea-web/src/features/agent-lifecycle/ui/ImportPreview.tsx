/**
 * What Import shows before the write: the team and its lead (a team zip), the
 * agents, the toolkit and skill counts, and what the file cannot carry. A
 * refusal stays here in the server's words; an invalid file's refusal
 * (422 `import_invalid`, issue 3137) also names the field it refused.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { ImportDocumentSummary } from '../lib/importDocument';
import type { ImportRefusal } from '../lib/importFollowUps';

export interface ImportPreviewProps {
  readonly summary: ImportDocumentSummary;
  readonly refusal: ImportRefusal | undefined;
  readonly testIdPrefix: string;
}

function SkippedList({ skipped }: { readonly skipped: readonly string[] }): ReactNode {
  if (skipped.length === 0) return null;
  return (
    <Box>
      <Typography variant="bodySmall">{t('features.agentLifecycle.import.skipped', 'Not imported (this file cannot carry them)')}:</Typography>
      {skipped.map((item) => (
        <Typography key={item} variant="bodySmall">
          {item}
        </Typography>
      ))}
    </Box>
  );
}

function ImportRefusalAlert({ refusal, testIdPrefix }: { readonly refusal: ImportRefusal; readonly testIdPrefix: string }): ReactNode {
  return (
    <Alert severity="error" data-testid={`${testIdPrefix}-import-refusal`}>
      {refusal.message}
      {refusal.field !== undefined && (
        <Typography variant="bodySmall" component="div" data-testid={`${testIdPrefix}-import-refusal-field`}>
          {t('features.agentLifecycle.import.refusedField', 'Field: {{field}}', { field: refusal.field })}
        </Typography>
      )}
    </Alert>
  );
}

export function ImportPreview({ summary, refusal, testIdPrefix }: ImportPreviewProps): ReactNode {
  return (
    <Box>
      {summary.team !== undefined && (
        <Box data-testid={`${testIdPrefix}-import-team`}>
          <Typography variant="labelMedium">{t('features.agentLifecycle.import.teamName', 'Team: {{name}}', { name: summary.team.name })}</Typography>
          {summary.team.lead !== undefined && (
            <Typography variant="bodySmall">{t('features.agentLifecycle.import.teamLead', 'Lead: {{name}}', { name: summary.team.lead })}</Typography>
          )}
        </Box>
      )}
      {summary.names.map((name) => (
        <Typography key={name} variant="bodyMedium">
          {name}
        </Typography>
      ))}
      <Typography variant="bodySmall">
        {t('features.agentLifecycle.import.toolkits', 'Toolkits')}: {summary.toolkitCount}
      </Typography>
      <Typography variant="bodySmall">
        {t('features.agentLifecycle.import.skills', 'Skills')}: {summary.skillCount}
      </Typography>
      <SkippedList skipped={summary.skipped} />
      {refusal !== undefined && <ImportRefusalAlert refusal={refusal} testIdPrefix={testIdPrefix} />}
    </Box>
  );
}
