import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { ApplicationReleaseChange } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The changes between two versions of an agent, one line each: the operation, the
 * JSON path and the new value. It renders an `agentrelease.Diff`, so a release
 * history and a Marketplace upgrade read the same way.
 */
export function ChangeList({ changes }: { readonly changes: readonly ApplicationReleaseChange[] }): ReactNode {
  if (changes.length === 0) {
    return (
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {t('features.agentLifecycle.releases.noChanges', 'No changes.')}
      </Typography>
    );
  }
  return (
    <Box
      component="ul"
      sx={{ margin: 0, paddingLeft: '1.25rem' }}
    >
      {changes.map((change) => (
        <Typography
          key={`${change.op}:${change.path}`}
          component="li"
          variant="bodySmall"
          sx={{ fontFamily: 'monospace', wordBreak: 'break-word' }}
        >
          {change.op} {change.path}
          {change.op !== 'remove' && `: ${preview(change.after)}`}
        </Typography>
      ))}
    </Box>
  );
}

/** A changed value, short enough for one line. */
function preview(value: unknown): string {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  if (text === undefined) return '';
  return text.length > 160 ? `${text.slice(0, 157)}...` : text;
}
