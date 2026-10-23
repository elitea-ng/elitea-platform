/**
 * The read-only sub-agents one `spawn_agent` call started (#1226), under its
 * tool row: one lane each, with its status, its own tool rows and text, and
 * its report. A lane is open while its child runs and folds once it settled.
 */
import { useState, type ComponentType } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import CircularProgress from '@mui/material/CircularProgress';
import Collapse from '@mui/material/Collapse';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { SubAgentStatus } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { SubAgentLane } from '../model/subAgentEvents';
import type { TranscriptItem } from '../model/transcriptItem';

type ToolItem = Extract<TranscriptItem, { type: 'tool' }>;

function statusLabel(status: SubAgentStatus): string {
  switch (status) {
    case 'running':
      return t('workspace.subagent.running', 'Running');
    case 'completed':
      return t('workspace.subagent.completed', 'Done');
    case 'failed':
      return t('workspace.subagent.failed', 'Failed');
    case 'budget_exhausted':
      return t('workspace.subagent.budgetExhausted', 'Out of budget');
    case 'cancelled':
      return t('workspace.subagent.cancelled', 'Stopped');
  }
}

function title(lane: SubAgentLane): string {
  const name = t('workspace.subagent.title', 'Sub-agent {{n}}', { n: lane.ordinal });
  return lane.label === undefined ? name : `${name} · ${lane.label}`;
}

function usage(lane: SubAgentLane): string | null {
  if (lane.steps === undefined) return null;
  return t('workspace.subagent.usage', '{{steps}} steps · {{tokens}} tokens', {
    steps: lane.steps,
    tokens: (lane.inputTokens ?? 0) + (lane.outputTokens ?? 0),
  });
}

function Lane({ lane, ToolRow }: { lane: SubAgentLane; ToolRow: ComponentType<{ item: ToolItem }> }): React.JSX.Element {
  const running = lane.status === 'running';
  // Open while it runs; once it settled, the person's choice.
  const [chosen, setChosen] = useState<boolean | null>(null);
  const open = chosen ?? running;
  const bodyId = `subagent-${lane.agentId}`;
  const spent = usage(lane);
  return (
    <Box
      data-testid="subagent-lane"
      sx={(theme: Theme) => ({ marginLeft: 3, paddingLeft: 1, borderLeft: `2px solid ${theme.vars.palette.divider}` })}
    >
      <ButtonBase
        aria-expanded={open}
        aria-controls={bodyId}
        onClick={() => setChosen(!open)}
        sx={(theme: Theme) => ({
          display: 'flex',
          alignItems: 'center',
          gap: 1,
          width: '100%',
          paddingX: 0.5,
          paddingY: 0.25,
          borderRadius: theme.vars.shape.radiusSm,
          '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
        })}
      >
        <Box aria-hidden sx={{ display: 'flex', '& svg': { width: '1rem', height: '1rem' } }}>
          {open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
        </Box>
        <Typography variant="labelSmall" component="span" sx={{ flex: 1, minWidth: 0, textAlign: 'left', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {title(lane)}
        </Typography>
        {spent !== null && (
          <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, flexShrink: 0 })}>
            {spent}
          </Typography>
        )}
        <Box component="output" sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexShrink: 0 }} data-testid="subagent-status">
          {running && <CircularProgress size="0.75rem" />}
          <Typography variant="bodySmall" component="span">
            {statusLabel(lane.status)}
          </Typography>
        </Box>
      </ButtonBase>
      <Collapse in={open} unmountOnExit>
        <Box id={bodyId} sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, marginTop: 0.5 }}>
          {lane.items.map((item) =>
            item.type === 'tool' ? (
              <ToolRow key={item.key} item={item} />
            ) : item.type === 'text' ? (
              <Typography key={item.key} variant="bodySmall" component="p" sx={(theme: Theme) => ({ margin: 0, whiteSpace: 'pre-wrap', color: theme.vars.palette.text.secondary })}>
                {item.text}
              </Typography>
            ) : null,
          )}
          {lane.result !== undefined && (
            <Typography
              variant="bodySmall"
              component="pre"
              data-testid="subagent-result"
              sx={(theme: Theme) => ({ whiteSpace: 'pre-wrap', margin: 0, padding: 1, fontFamily: theme.typography.fontFamilyMono, color: theme.vars.palette.text.secondary })}
            >
              {lane.result}
            </Typography>
          )}
        </Box>
      </Collapse>
    </Box>
  );
}

export interface SubAgentLanesProps {
  lanes: readonly SubAgentLane[];
  /** The transcript's own tool row, for the children's calls. */
  ToolRow: ComponentType<{ item: ToolItem }>;
}

export function SubAgentLanes({ lanes, ToolRow }: SubAgentLanesProps): React.JSX.Element | null {
  if (lanes.length === 0) return null;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }} data-testid="subagent-lanes">
      {lanes.map((lane) => (
        <Lane key={lane.agentId} lane={lane} ToolRow={ToolRow} />
      ))}
    </Box>
  );
}
