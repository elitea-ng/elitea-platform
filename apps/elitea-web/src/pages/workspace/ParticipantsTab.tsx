/**
 * A Local work thread's Participants tab in the inspector (ADR-0061 decision 2a, #3055): the chat's participants
 * panel, in the inspector's appearance and with the thread's agents only (no toolkit, pipeline or MCP sections).
 * With two or more agents it is the first tab (`team`), with one it follows the others (`participants`).
 *
 * Each agent row shows where it runs (Local or Cloud), the Lead chip, and its live state ("Running cargo",
 * decision 4c): only the active agent's avatar animates. Its menu makes or clears the lead, addresses the agent in
 * the composer (`@Name`), moves it here or to the cloud (not while the thread is handed off), opens the agent and
 * removes it from the thread. The exchange strip (decision 2c) sits on top while a message's agents answer in turn.
 *
 * The thread's agents are managed here (#3081), not in the composer's chip: below the conversation's rows,
 * `ThreadAgentsPanel` lists the agents that join with the next message (before the first send, the composer's agent and
 * the ones added to it, so a new thread has the tab too) and the Add agent, Add cloud agent and Add team pickers.
 *
 * Below them, "Thread settings" (#3145): the composer's `ThreadSettings` (where the thread works and runs, the sandbox,
 * the sub-agents, the step limit, each agent's model, the meters) rendered into `settingsRef`, then the thread's project
 * instructions (issue 3130): the folder's AGENTS.md, once, with the thread's "Respect AGENTS.md" switch
 * (`ProjectInstructionsSection`). Each row's "Model in this thread…" opens that agent's model there, and a row whose
 * agent runs on another model than its own says which.
 */
import { useEffect, useMemo, useRef, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { useNavigate } from '@tanstack/react-router';

import { applyLocalAgentState, useAgentAvatarStates } from '@/features/chat-messages';
import { ParticipantsWrapper } from '@/features/chat-participants';
import type { WorkspaceTurn } from '@/features/workspace';
import { InspectorTab } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';
import { agentAvatarRenderer, AgentStateAnnouncer, HandoffBaton, useAgentRowViews } from '@/pages/chat/agentPresence';
import { ParticipantRowControls, type ParticipantRowActions } from '@/pages/chat/ParticipantRowControls';

import type { AgentModels } from './agentModels';
import { ExchangeStrip, exchangeOf, type ExchangeStripProps } from './ExchangeStrip';
import { ProjectInstructionsSection, type ProjectInstructionsSectionProps } from './ProjectInstructionsSection';
import { participantsTabId, pendingAgents, ThreadAgentsPanel } from './ThreadAgentsPanel';
import type { AgentSelection } from './useAgentSelection';
import { useThreadTeam } from './useThreadTeam';

export interface ParticipantsTabProps {
  readonly workspaceId: string;
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
  readonly ipc: ExchangeStripProps['ipc'] & ProjectInstructionsSectionProps['ipc'];
  /** The composer's agents (#3081): the ones that join with the next message, and the add pickers. */
  readonly selection?: AgentSelection | undefined;
  /** Each agent's model in this thread (#3145): the rows' "Model in this thread…" and badge. */
  readonly models?: AgentModels | undefined;
  /** Where the composer renders "Thread settings" (#3145). */
  readonly settingsRef?: ((node: HTMLElement | null) => void) | undefined;
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

/** The rows' model actions (#3145): open the agent's choice in Thread settings, and the badge's model. */
function modelActions(agents: readonly { participantId: number; applicationId: number }[], models: AgentModels | undefined): Pick<ParticipantRowActions, 'onModel' | 'modelOf'> {
  if (models === undefined) return {};
  const applicationOf = (id: number): number | undefined => agents.find((row) => row.participantId === id)?.applicationId;
  return {
    onModel: (id) => {
      const applicationId = applicationOf(id);
      if (applicationId !== undefined) models.edit(applicationId);
    },
    modelOf: (id) => {
      const applicationId = applicationOf(id);
      return applicationId === undefined ? undefined : models.overrideOf(applicationId)?.name;
    },
  };
}

function report(action: string) {
  return (error: unknown) => { console.error(`[workspace] failed to ${action}`, error); };
}

export function ParticipantsTab({ workspaceId, projectId, conversationId, subject, executorLocked, onAddress, turn, ipc, selection, models, settingsRef }: ParticipantsTabProps): ReactNode {
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
    onRemove: (id) => {
      // An agent the composer still holds would join again with the next message.
      const agent = team.agents.find((row) => row.participantId === id);
      if (agent !== undefined) selection?.removeCompanion?.(String(agent.applicationId));
      team.remove(id).catch(report('remove the agent'));
    },
    ...modelActions(team.agents, models),
  }), [team, executorLocked, onAddress, navigate, selection, models]);
  const renderAgentLead = useMemo(() => ({ participantId }: { participantId: number }) => {
    const row = rows.find((candidate) => candidate.participantId === participantId);
    return row === undefined ? null : <ParticipantRowControls row={row} actions={actions} />;
  }, [rows, actions]);
  // Each agent's logo or live avatar in its row's avatar place (#3131).
  const participants = team.conversation?.participants;
  const renderAgentAvatar = useMemo(() => agentAvatarRenderer(participants, rows), [participants, rows]);
  const joined = team.conversation === undefined ? [] : team.agents;
  const pending = selection === undefined ? [] : pendingAgents(selection, joined);
  if (joined.length === 0 && pending.length === 0) return null;
  return (
    <InspectorTab id={participantsTabId(joined.length + pending.length)} title={t('workspace.participants.title', 'Participants')} subject={subject}>
      <Box ref={container} sx={{ position: 'relative', display: 'flex', flexDirection: 'column', minHeight: 0 }} data-testid="workspace-participants-tab">
        <ExchangeStrip exchange={exchangeOf(turn, team.agents)} ipc={ipc} />
        {joined.length > 0 && (
          <ParticipantsWrapper
            appearance="inspector"
            context="local_work"
            activeConversation={team.conversation as unknown as WrapperConversation}
            renderAgentLead={renderAgentLead}
            renderAgentAvatar={renderAgentAvatar}
          />
        )}
        {selection !== undefined && <ThreadAgentsPanel selection={selection} joined={joined} locked={executorLocked} />}
        <Box component="section" aria-labelledby="thread-settings-title" data-testid="thread-settings-section" sx={(theme: Theme) => ({ borderTop: `0.0625rem solid ${theme.vars.palette.divider}`, paddingTop: 1 })}>
          <Typography id="thread-settings-title" component="h2" variant="labelMedium" sx={{ paddingX: 2, margin: 0 }}>
            {t('workspace.settings.title', 'Thread settings')}
          </Typography>
          <Box ref={settingsRef} sx={{ paddingX: 2, paddingY: 1 }} />
          <ProjectInstructionsSection workspaceId={workspaceId} conversationId={conversationId} turn={turn} ipc={ipc} />
        </Box>
        <HandoffBaton container={container} states={states} />
        <AgentStateAnnouncer rows={rows} />
      </Box>
    </InspectorTab>
  );
}
