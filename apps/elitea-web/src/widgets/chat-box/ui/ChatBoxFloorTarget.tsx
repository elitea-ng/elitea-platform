/**
 * "To: X" above the composer (ADR-0050 decisions 7c/7d, issue 1612): which
 * agent answers a message that addresses no one, in a conversation with two
 * or more agents.
 *
 * With a lead (`meta.floor`), the lead answers, whatever agent is selected:
 * only an address overrides it, so picking another agent here addresses this
 * message to it (`@Name` in the composer). Without a lead, the selected or
 * sticky agent answers (`resolveTargetParticipant`), and a pick here selects
 * that agent, as the participants panel does.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { FLOOR_POLICY, floorOf, type AddressableAgent } from '@elitea/client-core/conversation';
import { t } from '@/shared/i18n';

import { resolveTargetParticipant } from './hooks/useChatBoxSend.helpers';

export interface FloorTargetInput {
  readonly agents: readonly AddressableAgent[];
  readonly meta: unknown;
  readonly activeParticipant: unknown;
  readonly participants: readonly unknown[] | undefined;
  readonly stickyParticipantId: string | undefined;
}

export interface FloorTarget {
  readonly agent: AddressableAgent;
  readonly isLead: boolean;
}

/** The agent that answers an unaddressed message, or `undefined` when the person has to pick (or there is one agent). */
export function floorTarget({ agents, meta, activeParticipant, participants, stickyParticipantId }: FloorTargetInput): FloorTarget | undefined {
  if (agents.length < 2) return undefined;
  const floor = floorOf(meta);
  if (floor.policy === FLOOR_POLICY.Lead) {
    const lead = agents.find((agent) => agent.participantId === floor.leadParticipantId);
    if (lead !== undefined) return { agent: lead, isLead: true };
  }
  const target = resolveTargetParticipant(activeParticipant, participants, stickyParticipantId) as { readonly id?: unknown } | null | undefined;
  const id = Number(target?.id);
  const agent = agents.find((candidate) => candidate.participantId === id);
  return agent === undefined ? undefined : { agent, isLead: false };
}

export interface ChatBoxFloorTargetProps extends FloorTargetInput {
  /** Selects the agent as the composer's target (no lead). */
  readonly onSelect: (participant: unknown) => void;
  /** Addresses the message to the agent (a lead is set). */
  readonly onAddress: (agent: AddressableAgent) => void;
}

function participantOf(participants: readonly unknown[] | undefined, agent: AddressableAgent): unknown {
  return (participants ?? []).find((row) => Number((row as { readonly id?: unknown } | null)?.id) === agent.participantId);
}

export function ChatBoxFloorTarget(props: ChatBoxFloorTargetProps): ReactNode {
  const { agents, participants, onSelect, onAddress } = props;
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  if (agents.length < 2) return null;
  const target = floorTarget(props);
  const name = target?.agent.name ?? t('widgets.chatBox.floorTarget.pick', 'pick an agent');
  const label = target?.isLead === true
    ? t('widgets.chatBox.floorTarget.lead', 'To: {{name}} (lead)', { name })
    : t('widgets.chatBox.floorTarget.label', 'To: {{name}}', { name });
  const pick = (agent: AddressableAgent) => {
    setAnchor(null);
    if (target?.isLead === true) { onAddress(agent); return; }
    const participant = participantOf(participants, agent);
    if (participant !== undefined) onSelect(participant);
  };
  return (
    <Box sx={{ display: 'flex', mb: 0.5 }}>
      <ButtonBase
        data-testid="chat-floor-target"
        aria-haspopup="menu"
        aria-label={t('widgets.chatBox.floorTarget.change', 'Change who answers')}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={(theme: Theme) => ({
          paddingX: 1,
          paddingY: 0.25,
          borderRadius: theme.vars.shape.radiusMd,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          color: theme.vars.palette.text.secondary,
        })}
      >
        <Typography variant="labelSmall" color="inherit">{label}</Typography>
      </ButtonBase>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)}>
        {agents.map((agent) => (
          <MenuItem
            key={agent.participantId}
            selected={agent.participantId === target?.agent.participantId}
            onClick={() => pick(agent)}
            data-testid="chat-floor-target-option"
          >
            {agent.name}
          </MenuItem>
        ))}
      </Menu>
    </Box>
  );
}
