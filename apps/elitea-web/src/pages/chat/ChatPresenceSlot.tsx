/**
 * The chat participants panel's agent rows with live presence (ADR-0061 decisions 4c and 6, #3067 #3069):
 *
 * - every agent row shows its state label ("Running jira_get_issue") from the stream's `agent_state` frames, and only
 *   the active agent's badge animates;
 * - a desktop Local work thread opened on the web is read-only (`readOnly`): the rows show where each agent runs
 *   (Local or Cloud) and the Lead chip, without the lead menu or any other edit;
 * - an ordinary chat keeps its lead menu (`AgentLeadControl`) beside the label.
 */
import { useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';

import { useAgentAvatarStates } from '@/features/chat-messages';
import { floorOf, FLOOR_POLICY } from '@elitea/client-core/conversation';

import { AgentStateAnnouncer, useAgentRowViews, type PresenceAgent } from './agentPresence';
import { ParticipantRowControls } from './ParticipantRowControls';

type LeadRenderer = (props: { conversationId: string | number; participantId: number }) => ReactNode;

/** What the participants panel takes from presence: the row slot, and the Local work appearance. */
export interface PresencePanelProps {
  readonly renderAgentLead?: LeadRenderer;
  readonly context?: 'local_work';
  readonly readOnly?: boolean;
}

const LOCAL_WORK_CONTEXT = 'local_work' as const;

interface PresenceConversation {
  readonly uuid?: string | undefined;
  readonly meta?: unknown;
  readonly participants?: readonly unknown[] | undefined;
}

interface AgentRow {
  readonly id?: unknown;
  readonly entity_name?: unknown;
  readonly entity_meta?: { readonly name?: unknown } | null;
  readonly entity_settings?: { readonly executor?: unknown } | null;
  readonly persona?: { readonly display_name?: unknown } | null;
}

const text = (value: unknown): string => (typeof value === 'string' ? value.trim() : '');

/** The conversation's agents, with where each runs. */
export function presenceAgents(participants: readonly unknown[] | undefined): PresenceAgent[] {
  const out: PresenceAgent[] = [];
  for (const participant of participants ?? []) {
    const row = (participant ?? {}) as AgentRow;
    const id = Number(row.id);
    if (row.entity_name !== 'application' || !Number.isSafeInteger(id) || id <= 0) continue;
    out.push({ participantId: id, name: text(row.persona?.display_name) || text(row.entity_meta?.name), cloud: row.entity_settings?.executor === 'cloud' });
  }
  return out;
}

function leadOf(meta: unknown): number {
  const floor = floorOf(meta);
  return floor.policy === FLOOR_POLICY.Lead ? floor.leadParticipantId : 0;
}

/**
 * The panel's `renderAgentLead` slot with presence, and the polite live region to render beside the panel. `lead` is
 * the chat's own lead slot (absent with one agent); `readOnly` drops it for a Local work thread.
 */
export function usePresenceSlot(conversation: PresenceConversation | undefined, lead: LeadRenderer | undefined, readOnly: boolean): { panelProps: PresencePanelProps; announcer: ReactNode } {
  const agents = useMemo(() => presenceAgents(conversation?.participants), [conversation?.participants]);
  const states = useAgentAvatarStates(conversation?.uuid === '' ? undefined : conversation?.uuid);
  const rows = useAgentRowViews(agents, states, leadOf(conversation?.meta));
  const renderAgentLead = useMemo<LeadRenderer | undefined>(() => {
    if (agents.length === 0) return lead;
    return (props) => {
      const row = rows.find((candidate) => candidate.participantId === props.participantId);
      if (row === undefined) return readOnly ? null : lead?.(props);
      if (readOnly) return <ParticipantRowControls row={row} />;
      // The wrapper never shrinks (as the lead control alone never did): with a long agent name a shrinking wrapper let
      // the lead menu button slide under the row's hover actions, where it could not be clicked. The label caps itself.
      return (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexShrink: 0 }} data-testid="participant-presence-lead">
          <ParticipantRowControls row={{ ...row, isLead: false }} presenceOnly />
          {lead?.(props)}
        </Box>
      );
    };
  }, [agents.length, rows, lead, readOnly]);
  const panelProps: PresencePanelProps = {
    ...(renderAgentLead === undefined ? {} : { renderAgentLead }),
    // A Local work thread holds agents only (no toolkit, pipeline or MCP sections), and the web edits none of it.
    ...(readOnly ? { context: LOCAL_WORK_CONTEXT, readOnly: true } : {}),
  };
  return { panelProps, announcer: <AgentStateAnnouncer rows={rows} /> };
}
