/**
 * "To: Name" above a Local work thread's composer (ADR-0061 decision 2b, #3056), as the chat shows it: in a thread of
 * two or more agents, who answers the message being typed, by the rule Main routes on (client-core `firstAnswerer`):
 * the first agent it addresses, else the lead, else the composer's agent (the sticky one). Picking another agent
 * addresses the message to it (`@Name` into the composer), the chat's chip under a lead.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { FLOOR_POLICY, addressedTo, firstAnswerer, floorOf } from '@elitea/client-core/conversation';
import { t } from '@/shared/i18n';

import { requestAddress, useThreadFloor } from './useComposerMentions';
import type { ThreadAgent } from './useEnsureConversation';

const NO_AGENTS: readonly ThreadAgent[] = [];

/** The composer's agents and To chip for a local thread (`conversationId` `''`: none, a new thread or the cloud copy). */
export function useComposerTeam(projectId: number, conversationId: string, local: boolean, agentId: string): { agents: readonly ThreadAgent[]; renderTo?: (draft: string) => ReactNode } {
  const floor = useThreadFloor(projectId, local ? conversationId : '');
  if (floor === undefined) return { agents: NO_AGENTS };
  const sticky = floor.agents.find((agent) => String(agent.applicationId) === agentId)?.participantId ?? 0;
  return {
    agents: floor.agents,
    renderTo: (draft) => (
      <WorkspaceToChip draft={draft} agents={floor.agents} meta={floor.meta} fallbackParticipantId={sticky} onAddress={(agent) => requestAddress(conversationId, agent.name)} />
    ),
  };
}

export interface ToTarget {
  readonly agent: ThreadAgent;
  /** It answers as the lead (the message names no agent). */
  readonly isLead: boolean;
}

/** Who answers `draft` first; `undefined` with fewer than two agents, or when the person has to pick. */
export function toTarget(draft: string, agents: readonly ThreadAgent[], meta: unknown, fallbackParticipantId: number): ToTarget | undefined {
  if (agents.length < 2) return undefined;
  const addressed = addressedTo(draft, agents);
  const floor = floorOf(meta);
  const id = firstAnswerer(addressed, floor, agents, fallbackParticipantId);
  const agent = agents.find((candidate) => candidate.participantId === id);
  if (agent === undefined) return undefined;
  return { agent, isLead: addressed.length === 0 && floor.policy === FLOOR_POLICY.Lead && floor.leadParticipantId === id };
}

export interface WorkspaceToChipProps {
  readonly draft: string;
  readonly agents: readonly ThreadAgent[];
  readonly meta: unknown;
  /** The composer's agent (an unaddressed message without a lead goes to it); 0 for none. */
  readonly fallbackParticipantId: number;
  readonly onAddress: (agent: ThreadAgent) => void;
}

export function WorkspaceToChip({ draft, agents, meta, fallbackParticipantId, onAddress }: WorkspaceToChipProps): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  if (agents.length < 2) return null;
  const target = toTarget(draft, agents, meta, fallbackParticipantId);
  const name = target?.agent.name ?? t('widgets.chatBox.floorTarget.pick', 'pick an agent');
  const label = target?.isLead === true
    ? t('widgets.chatBox.floorTarget.lead', 'To: {{name}} (lead)', { name })
    : t('widgets.chatBox.floorTarget.label', 'To: {{name}}', { name });
  return (
    <Box sx={{ display: 'flex' }}>
      <ButtonBase
        data-testid="workspace-to-chip"
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
        {agents.filter((agent) => agent.name !== '').map((agent) => (
          <MenuItem
            key={agent.participantId}
            selected={agent.participantId === target?.agent.participantId}
            onClick={() => { setAnchor(null); onAddress(agent); }}
            data-testid="workspace-to-chip-option"
          >
            {agent.name}
          </MenuItem>
        ))}
      </Menu>
    </Box>
  );
}
