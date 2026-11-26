/**
 * The exchange strip (ADR-0061 decision 2c, #3058): while a message addressed to several agents still has agents to
 * answer, "Now: Mira · Next: CI Doctor, Docs Writer" above the composer and in the Participants tab, with **Stop
 * exchange** (cancels the running turn; the host starts none of the rest) and **Skip** on each queued agent (the host
 * starts the next one). The host keeps the order (`status.floor.exchange`, then `exchange` events): the strip only
 * reads it. It is gone once the turn is done with no agent after it.
 */
import { useState, type ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { WorkspaceTurn } from '@/features/workspace';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { ThreadAgent } from './useEnsureConversation';

export interface ExchangeView {
  readonly turnId: string;
  readonly now: string;
  /** `handId`: the hand-off that queued the agent (ADR-0061 decision 3d), which its Skip names. */
  readonly next: readonly { readonly participantId: number; readonly name: string; readonly handId?: string }[];
}

/** What the strip shows for the turn on screen, or `undefined` when no exchange runs. */
export function exchangeOf(turn: Pick<WorkspaceTurn, 'turnId' | 'view'>, agents: readonly Pick<ThreadAgent, 'participantId' | 'name'>[]): ExchangeView | undefined {
  const agent = turn.view.agent;
  const remaining = agent?.exchange ?? [];
  if (turn.turnId === null || agent === undefined || remaining.length === 0) return undefined;
  // Done with no agent after it (a stop, an error): the strip goes.
  if (turn.view.done !== undefined && turn.view.done.continuing !== true) return undefined;
  if (turn.view.floorStopped !== undefined) return undefined;
  const nameOf = (id: number): string => agents.find((row) => row.participantId === id)?.name ?? t('chat.presence.anotherAgent', 'another agent');
  // The host's queue, each entry with its own hand-off; an older host sends only the ids.
  const queue = agent.queue ?? remaining.map((participantId) => ({ participantId }));
  return { turnId: turn.turnId, now: agent.name, next: queue.map((item) => ({ ...item, name: nameOf(item.participantId) })) };
}

export interface ExchangeStripProps {
  readonly exchange: ExchangeView | undefined;
  readonly ipc: Pick<WorkspaceIpc, 'stopExchange' | 'skipInExchange'>;
}

export function ExchangeStrip({ exchange, ipc }: ExchangeStripProps): ReactNode {
  const [error, setError] = useState<string | null>(null);
  if (exchange === undefined) return null;
  const fail = (cause: unknown) => { setError(cause instanceof Error ? cause.message : String(cause)); };
  const stop = () => { setError(null); ipc.stopExchange(exchange.turnId).catch(fail); };
  const skip = (participantId: number, handId: string | undefined) => { setError(null); ipc.skipInExchange(exchange.turnId, participantId, handId).catch(fail); };
  return (
    <Box
      data-testid="workspace-exchange-strip"
      component="section"
      aria-label={t('workspace.exchange.label', 'Agents answering in turn')}
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        flexWrap: 'wrap',
        gap: 0.5,
        paddingX: 1,
        paddingY: 0.5,
        borderRadius: theme.vars.shape.radiusMd,
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
      })}
    >
      <Typography variant="labelSmall" data-testid="workspace-exchange-now">{t('workspace.exchange.now', 'Now: {{name}}', { name: exchange.now })}</Typography>
      <Typography variant="labelSmall" color="text.secondary">{t('workspace.exchange.next', 'Next:')}</Typography>
      {exchange.next.map((agent, index) => (
        <Chip
          // An agent may be queued twice: the place in the queue tells the chips apart.
          key={`${String(index)}:${String(agent.participantId)}`}
          size="small"
          variant="outlined"
          label={agent.name}
          data-testid="workspace-exchange-next"
          data-participant-id={agent.participantId}
          onDelete={() => skip(agent.participantId, agent.handId)}
          deleteIcon={<CloseIcon aria-label={t('workspace.exchange.skip', 'Skip {{name}}', { name: agent.name })} data-testid="workspace-exchange-skip" />}
        />
      ))}
      <Box sx={{ flex: 1 }} />
      <Button size="small" color="inherit" onClick={stop} data-testid="workspace-exchange-stop">{t('workspace.exchange.stop', 'Stop exchange')}</Button>
      {error !== null && <Typography variant="labelSmall" color="error" role="alert" sx={{ width: '100%' }}>{error}</Typography>}
    </Box>
  );
}
