/**
 * A Local work thread's Participants tab in the inspector (ADR-0061 decision 2a, #3055): the chat's participants
 * panel, in the inspector's appearance and with the thread's agents only (no toolkit, pipeline or MCP sections).
 * With two or more agents it is the first tab (`team`), with one it follows the others (`participants`).
 *
 * Each agent row shows where it runs (Local or Cloud), the Lead chip, and its live state ("Running cargo",
 * decision 4c): only the active agent's avatar animates. Its menu makes or clears the lead, addresses the agent in
 * the composer (`@Name`), moves it here or to the cloud (not while the thread is handed off), opens the agent and
 * removes it from the thread. The exchange strip (decision 2c) sits on top while a message's agents answer in turn.
 */
import { useEffect, useMemo, useRef, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import { useNavigate } from '@tanstack/react-router';

import { applyLocalAgentState, useAgentAvatarStates } from '@/features/chat-messages';
import { ParticipantsWrapper } from '@/features/chat-participants';
import type { WorkspaceTurn } from '@/features/workspace';
import { InspectorTab } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';
import { AgentStateAnnouncer, HandoffBaton, useAgentRowViews } from '@/pages/chat/agentPresence';
import { ParticipantRowControls, type ParticipantRowActions } from '@/pages/chat/ParticipantRowControls';

import { ExchangeStrip, exchangeOf, type ExchangeStripProps } from './ExchangeStrip';
import { useThreadTeam } from './useThreadTeam';

export interface ParticipantsTabProps {
  readonly projectId: number;
  readonly conversationId: string;
  /** What the tab shows (the thread's title): its own window is named after it. */
  readonly subject: string;
  /** The thread is handed off (ADR-0034): where an agent runs cannot change. */
  readonly executorLocked: boolean;
  /** Puts `@Name ` into the composer. */
  readonly onAddress: (name: string) => void;
  /** The turn on screen and the host: the exchange strip, while a message's agents answer in turn, and the live states. */
  readonly turn: Pick<WorkspaceTurn, 'turnId' | 'view'>;
  readonly ipc: ExchangeStripProps['ipc'];
}

type WrapperConversation = NonNullable<Parameters<typeof ParticipantsWrapper>[0]['activeConversation']>;

/**
 * Records the turn's live `agent_state` events (ADR-0061 decision 4b, #3057) in the avatar store under the thread's
 * conversation, as the chat records a stream's frames: each new state of the turn on screen, for its agent.
 */
function useLocalAgentStates(conversationId: string, agentState: WorkspaceTurn['view']['agentState']): void {
  useEffect(() => {
    if (agentState !== undefined) applyLocalAgentState(conversationId, agentState);
  }, [conversationId, agentState]);
}

function report(action: string) {
  return (error: unknown) => { console.error(`[workspace] failed to ${action}`, error); };
}

export function ParticipantsTab({ projectId, conversationId, subject, executorLocked, onAddress, turn, ipc }: ParticipantsTabProps): ReactNode {
  const navigate = useNavigate();
  const team = useThreadTeam({ projectId, conversationId });
  useLocalAgentStates(conversationId, turn.view.agentState);
  const states = useAgentAvatarStates(conversationId === '' ? undefined : conversationId);
  const rows = useAgentRowViews(team.agents, states, team.leadParticipantId);
  const container = useRef<HTMLDivElement | null>(null);
  const actions: ParticipantRowActions = useMemo(() => ({
    canLead: team.agents.length >= 2,
    executorLocked,
    disabled: team.saving,
    onSetLead: (id) => { team.setLead(id).catch(report('save the lead')); },
    onAddress,
    onSetCloud: (id, cloud) => { team.setCloud(id, cloud).catch(report('move the agent')); },
    onEdit: (id) => {
      const agent = team.agents.find((row) => row.participantId === id);
      if (agent !== undefined) void navigate({ to: '/agents/$tab/$agentId', params: { tab: 'all', agentId: String(agent.applicationId) } });
    },
    onRemove: (id) => { team.remove(id).catch(report('remove the agent')); },
  }), [team, executorLocked, onAddress, navigate]);
  const renderAgentLead = useMemo(() => ({ participantId }: { participantId: number }) => {
    const row = rows.find((candidate) => candidate.participantId === participantId);
    return row === undefined ? null : <ParticipantRowControls row={row} actions={actions} />;
  }, [rows, actions]);
  if (team.conversation === undefined || team.agents.length === 0) return null;
  const several = team.agents.length >= 2;
  return (
    <InspectorTab id={several ? 'team' : 'participants'} title={t('workspace.participants.title', 'Participants')} subject={subject}>
      <Box ref={container} sx={{ position: 'relative', display: 'flex', flexDirection: 'column', minHeight: 0 }} data-testid="workspace-participants-tab">
        <ExchangeStrip exchange={exchangeOf(turn, team.agents)} ipc={ipc} />
        <ParticipantsWrapper
          appearance="inspector"
          context="local_work"
          activeConversation={team.conversation as unknown as WrapperConversation}
          renderAgentLead={renderAgentLead}
        />
        <HandoffBaton container={container} states={states} />
        <AgentStateAnnouncer rows={rows} />
      </Box>
    </InspectorTab>
  );
}
