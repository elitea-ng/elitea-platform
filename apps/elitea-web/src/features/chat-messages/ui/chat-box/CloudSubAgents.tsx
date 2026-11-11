/**
 * A cloud answer's temporary sub-agents (client contract 1.48, ADR-0050
 * decision 4), under the answer's thinking: one row per child with its
 * dashed-ring avatar, its profile name, a "Temporary" badge and its status;
 * opened, its own tool calls and its final answer. Open while the child runs.
 *
 * Every text here is untrusted model or tool output and renders as plain
 * text (no Markdown, no HTML).
 */
import { useState, type ReactNode } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import CircularProgress from '@mui/material/CircularProgress';
import Collapse from '@mui/material/Collapse';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { CloudSubAgentLane, SubAgentLaneStatus } from '@elitea/client-core/stream';

import { t } from '@/shared/i18n';

import { AgentAvatar } from './AgentAvatar';

function statusLabel(status: SubAgentLaneStatus): string {
  switch (status) {
    case 'running':
      return t('features.chatMessages.subAgents.running', 'Running');
    case 'completed':
      return t('features.chatMessages.subAgents.completed', 'Done');
    case 'failed':
      return t('features.chatMessages.subAgents.failed', 'Failed');
    case 'budget_exhausted':
      return t('features.chatMessages.subAgents.budgetExhausted', 'Out of budget');
    case 'cancelled':
      return t('features.chatMessages.subAgents.cancelled', 'Stopped');
  }
}

function nameOf(lane: CloudSubAgentLane): string {
  const name = lane.displayName ?? t('features.chatMessages.subAgents.title', 'Sub-agent {{n}}', { n: lane.ordinal });
  return lane.label === undefined || lane.label === lane.displayName ? name : `${name} · ${lane.label}`;
}

function toolLine(item: Extract<CloudSubAgentLane['items'][number], { type: 'tool' }>): string {
  const state = item.ok === undefined ? '…' : item.ok ? '✓' : '✗';
  const args = item.argsSummary === undefined ? '' : ` ${item.argsSummary}`;
  const result = item.summary === undefined ? '' : ` → ${item.summary}`;
  return `${state} ${item.tool}${args}${result}`;
}

const mutedSx = (theme: Theme) => ({ margin: 0, whiteSpace: 'pre-wrap' as const, overflowWrap: 'anywhere' as const, color: theme.vars.palette.text.secondary });

function Lane({ lane }: { readonly lane: CloudSubAgentLane }): ReactNode {
  const running = lane.status === 'running';
  const [chosen, setChosen] = useState<boolean | null>(null);
  const open = chosen ?? running;
  const bodyId = `cloud-subagent-${lane.agentId}`;
  return (
    <Box data-testid="cloud-subagent" sx={(theme: Theme) => ({ paddingLeft: 1, borderLeft: `2px dashed ${theme.vars.palette.divider}` })}>
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
        <AgentAvatar ephemeral size="1rem" />
        <Typography variant="labelSmall" component="span" sx={{ flex: 1, minWidth: 0, textAlign: 'left', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {nameOf(lane)}
        </Typography>
        <Typography
          variant="bodySmall"
          component="span"
          data-testid="cloud-subagent-temporary"
          sx={(theme: Theme) => ({ flexShrink: 0, paddingX: 0.5, borderRadius: theme.vars.shape.radiusSm, border: `1px dashed ${theme.vars.palette.divider}`, color: theme.vars.palette.text.secondary })}
        >
          {t('features.chatMessages.subAgents.temporary', 'Temporary')}
        </Typography>
        <Box component="output" sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexShrink: 0 }} data-testid="cloud-subagent-status">
          {running && <CircularProgress size="0.75rem" />}
          <Typography variant="bodySmall" component="span">
            {statusLabel(lane.status)}
          </Typography>
        </Box>
      </ButtonBase>
      <Collapse in={open} unmountOnExit>
        <Box id={bodyId} sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, marginTop: 0.5, paddingX: 0.5 }}>
          {lane.items.map((item, index) => (
            <Typography key={item.type === 'tool' ? `c${item.callId}` : `t${String(index)}`} variant="bodySmall" component="p" sx={mutedSx}>
              {item.type === 'tool' ? toolLine(item) : item.text}
            </Typography>
          ))}
          {lane.result !== undefined && (
            <Typography variant="bodySmall" component="pre" data-testid="cloud-subagent-result" sx={(theme: Theme) => ({ ...mutedSx(theme), padding: 1, fontFamily: theme.typography.fontFamilyMono })}>
              {lane.result}
            </Typography>
          )}
        </Box>
      </Collapse>
    </Box>
  );
}

export function CloudSubAgents({ lanes }: { readonly lanes: readonly CloudSubAgentLane[] | undefined }): ReactNode {
  if (lanes === undefined || lanes.length === 0) return null;
  return (
    <Box data-testid="cloud-subagents" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, mb: 0.5 }}>
      {lanes.map((lane) => (
        <Lane key={lane.agentId} lane={lane} />
      ))}
    </Box>
  );
}
