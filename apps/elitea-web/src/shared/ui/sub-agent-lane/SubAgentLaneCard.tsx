/**
 * One sub-agent lane (issue 1924): the row a `spawn_agent` child gets, with
 * its avatar, name, "Temporary" badge, steps and tokens, status, and, opened,
 * its own items and its report. Open while the child runs; once it settled,
 * the person's choice.
 *
 * The ONE lane both hosts draw: the desktop workspace transcript
 * (`features/workspace`'s `SubAgentLanes`, a local or cloud child under its
 * tool row) and the cloud chat answer (`features/chat-messages`'
 * `CloudSubAgents`). The host renders the lane's items (its tool row
 * differs) and its avatar (`AgentAvatar` lives in a feature); the header,
 * the strings and the report are this file's, under one set of keys.
 *
 * Every text here is untrusted model or tool output: React renders it as text.
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

import { t } from '@/shared/i18n';

/** A lane's status (`ChildStatus` of the runtime). */
type SubAgentLaneStatus = 'running' | 'completed' | 'failed' | 'budget_exhausted' | 'cancelled';

/** What the lane's header and report show; both hosts' lanes have this shape. */
interface SubAgentLaneSummary {
  readonly agentId: string;
  readonly ordinal: number;
  readonly label?: string | undefined;
  /** The spawn profile's name; absent when it named none. */
  readonly displayName?: string | undefined;
  /** A cloud child (ADR-0050 decision 4): the lane says it is a temporary participant. */
  readonly temporary?: boolean | undefined;
  readonly status: SubAgentLaneStatus;
  readonly steps?: number | undefined;
  readonly inputTokens?: number | undefined;
  readonly outputTokens?: number | undefined;
  /** Its final report (or the text it had when it stopped). */
  readonly result?: string | undefined;
}

/**
 * Where the lane sits: `transcript` under a tool row of the workspace
 * transcript (indented, a solid rule), `answer` under a cloud chat answer's
 * thinking (a dashed rule). Each keeps its own test ids.
 */
type SubAgentLaneVariant = 'transcript' | 'answer';

export interface SubAgentLaneCardProps {
  readonly lane: SubAgentLaneSummary;
  readonly variant: SubAgentLaneVariant;
  /** The host's avatar for the lane; absent draws none. */
  readonly avatar?: ReactNode;
  /** The lane's own tool rows and text, in order, as the host renders them. */
  readonly children?: ReactNode;
}

function statusLabel(status: SubAgentLaneStatus): string {
  switch (status) {
    case 'running':
      return t('shared.ui.subAgentLane.running', 'Running');
    case 'completed':
      return t('shared.ui.subAgentLane.completed', 'Done');
    case 'failed':
      return t('shared.ui.subAgentLane.failed', 'Failed');
    case 'budget_exhausted':
      return t('shared.ui.subAgentLane.budgetExhausted', 'Out of budget');
    case 'cancelled':
      return t('shared.ui.subAgentLane.cancelled', 'Stopped');
  }
}

/** The profile's name (else "Sub-agent n"), and the spawn's label when it says something else. */
function subAgentLaneTitle(lane: SubAgentLaneSummary): string {
  const name = lane.displayName ?? t('shared.ui.subAgentLane.title', 'Sub-agent {{n}}', { n: lane.ordinal });
  return lane.label === undefined || lane.label === lane.displayName ? name : `${name} · ${lane.label}`;
}

function usage(lane: SubAgentLaneSummary): string | null {
  if (lane.steps === undefined) return null;
  return t('shared.ui.subAgentLane.usage', '{{steps}} steps · {{tokens}} tokens', {
    steps: lane.steps,
    tokens: (lane.inputTokens ?? 0) + (lane.outputTokens ?? 0),
  });
}

const TEST_IDS: Readonly<Record<SubAgentLaneVariant, { readonly prefix: string; readonly root: string }>> = {
  transcript: { prefix: 'subagent', root: 'subagent-lane' },
  answer: { prefix: 'cloud-subagent', root: 'cloud-subagent' },
};

const rootSx = (variant: SubAgentLaneVariant) => (theme: Theme) =>
  variant === 'transcript'
    ? { marginLeft: 3, paddingLeft: 1, borderLeft: `2px solid ${theme.vars.palette.divider}` }
    : { paddingLeft: 1, borderLeft: `2px dashed ${theme.vars.palette.divider}` };

const headerSx = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: 1,
  width: '100%',
  paddingX: 0.5,
  paddingY: 0.25,
  borderRadius: theme.vars.shape.radiusSm,
  '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
});

const badgeSx = (theme: Theme) => ({
  flexShrink: 0,
  paddingX: 0.5,
  borderRadius: theme.vars.shape.radiusSm,
  border: `1px dashed ${theme.vars.palette.divider}`,
  color: theme.vars.palette.text.secondary,
});

const resultSx = (theme: Theme) => ({
  whiteSpace: 'pre-wrap' as const,
  overflowWrap: 'anywhere' as const,
  margin: 0,
  padding: 1,
  fontFamily: theme.typography.fontFamilyMono,
  color: theme.vars.palette.text.secondary,
});

const nameSx = { flex: 1, minWidth: 0, textAlign: 'left', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

export function SubAgentLaneCard({ lane, variant, avatar, children }: SubAgentLaneCardProps): ReactNode {
  const running = lane.status === 'running';
  const [chosen, setChosen] = useState<boolean | null>(null);
  const open = chosen ?? running;
  const ids = TEST_IDS[variant];
  const bodyId = `${ids.prefix}-${lane.agentId}`;
  const spent = usage(lane);
  return (
    <Box data-testid={ids.root} sx={rootSx(variant)}>
      <ButtonBase aria-expanded={open} aria-controls={bodyId} onClick={() => setChosen(!open)} sx={headerSx}>
        <Box aria-hidden sx={{ display: 'flex', '& svg': { width: '1rem', height: '1rem' } }}>
          {open ? <ExpandMoreIcon /> : <ChevronRightIcon />}
        </Box>
        {avatar}
        <Typography variant="labelSmall" component="span" sx={nameSx}>
          {subAgentLaneTitle(lane)}
        </Typography>
        {lane.temporary === true && (
          <Typography variant="bodySmall" component="span" data-testid={`${ids.prefix}-temporary`} sx={badgeSx}>
            {t('shared.ui.subAgentLane.temporary', 'Temporary')}
          </Typography>
        )}
        {spent !== null && (
          <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, flexShrink: 0 })}>
            {spent}
          </Typography>
        )}
        <Box component="output" sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexShrink: 0 }} data-testid={`${ids.prefix}-status`}>
          {running && <CircularProgress size="0.75rem" />}
          <Typography variant="bodySmall" component="span">
            {statusLabel(lane.status)}
          </Typography>
        </Box>
      </ButtonBase>
      <Collapse in={open} unmountOnExit>
        <Box id={bodyId} sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, marginTop: 0.5, ...(variant === 'answer' ? { paddingX: 0.5 } : {}) }}>
          {children}
          {lane.result !== undefined && (
            <Typography variant="bodySmall" component="pre" data-testid={`${ids.prefix}-result`} sx={resultSx}>
              {lane.result}
            </Typography>
          )}
        </Box>
      </Collapse>
    </Box>
  );
}
