/**
 * A `run_command` row's background process (#1223): its state beside the
 * row, and the last lines of its output under the result (escape codes
 * dropped; the Processes tab shows the live terminal).
 */
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { ToolProcess } from '../model/transcriptItem';

function processStateLabel(process: ToolProcess): string {
  switch (process.status) {
    case 'running':
      return t('workspace.process.running', 'Running in the background');
    case 'exited':
      return t('workspace.process.exited', 'Exited ({{code}})', { code: String(process.exitCode ?? '?') });
    case 'killed':
      return t('workspace.process.killed', 'Stopped');
    case 'timed_out':
      return t('workspace.process.timedOut', 'Timed out');
  }
}

export function ToolProcessOutput({ process }: { process: ToolProcess }): React.JSX.Element {
  return (
    <Box data-testid="tool-process" sx={{ marginLeft: 3, marginTop: 0.5, display: 'flex', flexDirection: 'column', gap: 0.25 }}>
      <Typography variant="labelSmall" component="output" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {processStateLabel(process)}
      </Typography>
      {process.output !== '' && (
        <Typography
          variant="bodySmall"
          component="pre"
          data-testid="tool-process-output"
          sx={(theme: Theme) => ({
            whiteSpace: 'pre-wrap',
            margin: 0,
            padding: 1,
            maxHeight: '12rem',
            overflowY: 'auto',
            fontFamily: theme.typography.fontFamilyMono,
            borderLeft: `2px solid ${theme.vars.palette.divider}`,
            color: theme.vars.palette.text.secondary,
          })}
        >
          {process.output}
        </Typography>
      )}
    </Box>
  );
}
