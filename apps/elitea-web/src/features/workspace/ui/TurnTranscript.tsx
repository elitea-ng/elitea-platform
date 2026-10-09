/**
 * The live transcript of one turn: streamed text, tool rows (local or remote,
 * with a collapsible result), the status line and the cancel button.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Collapse from '@mui/material/Collapse';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { isTerminalPhase, type TranscriptItem, type TurnView } from '../model/turnReducer';

type ToolItem = Extract<TranscriptItem, { type: 'tool' }>;

function phaseLabel(phase: NonNullable<TurnView['phase']>): string {
  switch (phase) {
    case 'resolving':
      return t('workspace.phase.resolving', 'Preparing');
    case 'starting':
      return t('workspace.phase.starting', 'Starting');
    case 'running':
      return t('workspace.phase.running', 'Working');
    case 'committing':
      return t('workspace.phase.committing', 'Saving to the conversation');
    case 'done':
      return t('workspace.phase.done', 'Done');
    case 'cancelled':
      return t('workspace.phase.cancelled', 'Cancelled');
    case 'error':
      return t('workspace.phase.error', 'Failed');
  }
}

function ToolRow({ item }: { item: ToolItem }): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const { result } = item;
  const resultId = `tool-result-${item.callId}`;
  return (
    <Box
      data-testid="tool-row"
      sx={(theme: Theme) => ({
        border: `1px solid ${theme.vars.palette.divider}`,
        borderRadius: theme.vars.shape.radiusMd,
        padding: 1,
      })}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <Chip
          size="small"
          label={item.remote ? t('workspace.tool.remote', 'Remote') : t('workspace.tool.local', 'Local')}
          color={item.remote ? 'default' : 'primary'}
          variant="outlined"
        />
        <Typography variant="labelMedium">{item.tool}</Typography>
        <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary, wordBreak: 'break-all' }}>
          {item.argsSummary}
        </Typography>
        {result === undefined ? (
          <Typography variant="bodySmall" component="output">
            {t('workspace.tool.running', 'Running')}
          </Typography>
        ) : (
          <Button size="small" aria-expanded={open} aria-controls={resultId} onClick={() => setOpen((value) => !value)}>
            {result.ok ? t('workspace.tool.succeeded', 'Succeeded') : t('workspace.tool.failed', 'Failed')}
          </Button>
        )}
      </Box>
      {result !== undefined && (
        <Collapse in={open} unmountOnExit>
          <Typography id={resultId} variant="bodySmall" component="pre" sx={{ whiteSpace: 'pre-wrap', margin: 0, paddingTop: 1 }}>
            {result.summary}
            {result.truncated && ` ${t('workspace.tool.truncated', '(output truncated)')}`}
          </Typography>
        </Collapse>
      )}
    </Box>
  );
}

export interface TurnTranscriptProps {
  view: TurnView;
  busy: boolean;
  onCancel: () => void;
}

export function TurnTranscript({ view, busy, onCancel }: TurnTranscriptProps): React.JSX.Element {
  const showStatus = view.phase !== null;
  return (
    <Box data-testid="turn-transcript" sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      {view.items.map((item) =>
        item.type === 'text' ? (
          <Typography key={item.key} variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap' }}>
            {item.text}
          </Typography>
        ) : (
          <ToolRow key={item.key} item={item} />
        ),
      )}
      {view.error !== undefined && (
        <Alert severity="error">{view.error.message}</Alert>
      )}
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
        {view.projectInstructions !== undefined && view.projectInstructions.length > 0 && (
          <Tooltip title={view.projectInstructions.join(', ')}>
            <Chip
              size="small"
              variant="outlined"
              data-testid="agents-md-applied"
              label={t('workspace.agentsMdApplied', 'AGENTS.md applied')}
              aria-description={view.projectInstructions.join(', ')}
            />
          </Tooltip>
        )}
        {showStatus && view.phase !== null && (
          <Typography variant="bodySmall" component="output" data-testid="turn-status">
            {phaseLabel(view.phase)}
            {view.message !== undefined && ` — ${view.message}`}
          </Typography>
        )}
        {busy && !isTerminalPhase(view.phase) && (
          <Button size="small" color="error" variant="outlined" onClick={onCancel}>
            {t('workspace.cancel', 'Cancel')}
          </Button>
        )}
      </Box>
    </Box>
  );
}
