/**
 * The live transcript of one turn: streamed text, tool rows (local or remote,
 * with a collapsible result), the status line and the cancel button.
 */
import { useState } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import CloseIcon from '@mui/icons-material/Close';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
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

function ToolStatus({ result }: { result: ToolItem['result'] }): React.JSX.Element {
  if (result === undefined) {
    return (
      <Box component="output" sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
        <CircularProgress size="0.75rem" />
        <Typography variant="bodySmall" component="span">
          {t('workspace.tool.running', 'Running')}
        </Typography>
      </Box>
    );
  }
  return (
    <Box
      component="span"
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 0.5,
        color: result.ok ? theme.vars.palette.success.main : theme.vars.palette.error.main,
        '& svg': { width: '0.875rem', height: '0.875rem' },
      })}
    >
      {result.ok ? <CheckIcon /> : <CloseIcon />}
      <Typography variant="bodySmall" component="span">
        {result.ok ? t('workspace.tool.succeeded', 'Succeeded') : t('workspace.tool.failed', 'Failed')}
      </Typography>
    </Box>
  );
}

/** One tool call as a single line ("▸ tool  args  ✓"); the result unfolds under it. */
function ToolRow({ item }: { item: ToolItem }): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const { result } = item;
  const resultId = `tool-result-${item.callId}`;
  const line = (
    <>
      <Box aria-hidden sx={{ display: 'flex', '& svg': { width: '1rem', height: '1rem' } }}>
        {result === undefined ? <ChevronRightIcon sx={{ opacity: 0.4 }} /> : open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
      </Box>
      <Typography variant="labelSmall" component="span" sx={(theme: Theme) => ({ fontFamily: theme.typography.fontFamilyMono, flexShrink: 0 })}>
        {item.tool}
      </Typography>
      <Typography
        variant="bodySmall"
        component="span"
        sx={(theme: Theme) => ({
          color: theme.vars.palette.text.secondary,
          flex: 1,
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          textAlign: 'left',
        })}
      >
        {item.argsSummary}
      </Typography>
      <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, flexShrink: 0 })}>
        {item.remote ? t('workspace.tool.remote', 'Remote') : t('workspace.tool.local', 'Local')}
      </Typography>
      <ToolStatus result={result} />
    </>
  );
  const lineSx = (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: 1,
    width: '100%',
    paddingX: 0.5,
    paddingY: 0.25,
    borderRadius: theme.vars.shape.radiusSm,
  });
  return (
    <Box data-testid="tool-row">
      {result === undefined ? (
        <Box sx={lineSx}>{line}</Box>
      ) : (
        <ButtonBase
          aria-expanded={open}
          aria-controls={resultId}
          onClick={() => setOpen((value) => !value)}
          sx={(theme: Theme) => ({ ...lineSx(theme), '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover } })}
        >
          {line}
        </ButtonBase>
      )}
      {result !== undefined && (
        <Collapse in={open} unmountOnExit>
          <Typography
            id={resultId}
            variant="bodySmall"
            component="pre"
            sx={(theme: Theme) => ({
              whiteSpace: 'pre-wrap',
              margin: 0,
              marginLeft: 3,
              marginTop: 0.5,
              padding: 1,
              fontFamily: theme.typography.fontFamilyMono,
              borderLeft: `2px solid ${theme.vars.palette.divider}`,
              color: theme.vars.palette.text.secondary,
            })}
          >
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
    <Box data-testid="turn-transcript" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {view.items.map((item) =>
        item.type === 'text' ? (
          <Typography key={item.key} variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap', lineHeight: 1.6 }}>
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
