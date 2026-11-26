/**
 * Managing a Local work thread's agents from its Participants tab (#3081), as a chat's participants are managed in its
 * side panel, instead of nested menus of the composer's agent chip:
 *
 * - the agents that join with the next message (`pending`): before the first send the composer's agent (the lead, with
 *   its version and Change) and the agents added to it; later, the agents added since. Each row says where it runs,
 *   Lead on the lead, and has Make lead (before the first send) and Remove;
 * - **Add agent**, **Add cloud agent** and **Add team**, each a search picker. A team starts a new thread with the team's
 *   agents and lead (a team never joins a thread that exists), and they appear as rows here.
 *
 * The agents already in the conversation are the chat panel's rows above this (`ParticipantsTab`).
 */
import AddIcon from '@mui/icons-material/Add';
import CloudOutlinedIcon from '@mui/icons-material/CloudOutlined';
import GroupsOutlinedIcon from '@mui/icons-material/GroupsOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { useState } from 'react';

import { t } from '@/shared/i18n';

import { SearchPicker, type PickerOption } from './SearchPicker';
import { isCloudOnlyPick, type AgentSelection } from './useAgentSelection';
import type { ThreadAgent } from './useEnsureConversation';
import type { TeamUnavailable } from './workTeams';

/** One agent that joins with the next message. */
interface PendingAgent {
  readonly agentId: string;
  readonly name: string;
  readonly version: string;
  readonly cloud: boolean;
  /** The composer's agent of a new thread. */
  readonly lead: boolean;
}

/** The Participants tab's id: `team` (shown first) with two or more agents, `participants` with one. */
export function participantsTabId(agentCount: number): 'team' | 'participants' {
  return agentCount >= 2 ? 'team' : 'participants';
}

/** Why a team cannot run a thread here, as its row says. */
function unavailableText(reason: TeamUnavailable): string {
  switch (reason) {
    case 'other_project':
      return t('workspace.team.otherProject', 'Agents of another project');
    case 'not_listed':
      return t('workspace.team.notListed', 'An agent is not listed here');
    case 'pipeline':
      return t('workspace.team.pipeline', 'Has a pipeline');
    case 'no_members':
      return t('workspace.team.noMembers', 'No agents');
  }
}

/**
 * The agents that join with the next message: the composer's agent and its companions before the first send; after
 * it, the companions the conversation (`joined`) does not hold yet.
 */
export function pendingAgents(selection: AgentSelection, joined: readonly ThreadAgent[]): PendingAgent[] {
  const inThread = new Set(joined.map((agent) => String(agent.applicationId)));
  const companions = (selection.companions ?? [])
    .filter((c) => c.agentId !== selection.agentId && !inThread.has(c.agentId))
    .map((c): PendingAgent => ({ agentId: c.agentId, name: c.name, version: c.versionName ?? '', cloud: c.cloud, lead: false }));
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  // A thread that exists has its agents in the conversation (still loading, or the rows above).
  if (joined.length > 0 || selection.conversationId !== '' || agent === undefined) return companions;
  const version = selection.versions.find((v) => String(v.id) === selection.versionId)?.name ?? '';
  return [{ agentId: agent.id, name: agent.name, version, cloud: isCloudOnlyPick(selection), lead: true }, ...companions];
}

/** The agents "Add agent" (`cloud` false) or "Add cloud agent" offers: not the lead, not a pipeline, not yet added so. */
function addOptions(selection: AgentSelection, joined: readonly ThreadAgent[], cloud: boolean): PickerOption[] {
  const added = selection.companions ?? [];
  return selection.agents
    .filter((a) => a.id !== selection.agentId && selection.pipelineIds?.has(a.id) !== true)
    .filter((a) => !joined.some((j) => String(j.applicationId) === a.id && j.cloud === cloud))
    .filter((a) => !added.some((c) => c.agentId === a.id && c.cloud === cloud))
    .map((a) => ({ id: a.id, label: a.name }));
}

function teamOptions(selection: AgentSelection): PickerOption[] {
  return (selection.teams ?? []).map((choice) => ('team' in choice ? { id: String(choice.id), label: choice.name } : { id: String(choice.id), label: choice.name, unavailable: unavailableText(choice.unavailable) }));
}

/** The lead row's version: a menu of the agent's versions, before the first send. */
function VersionChoice({ selection }: { readonly selection: AgentSelection }): React.JSX.Element | null {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const current = selection.versions.find((v) => String(v.id) === selection.versionId);
  if (current === undefined) return null;
  return (
    <>
      <Button size="small" variant="text" data-testid="thread-lead-version" aria-haspopup="menu" disabled={selection.versions.length < 2} onClick={(event) => setAnchor(event.currentTarget)}>
        {current.name}
      </Button>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)}>
        {selection.versions.map((v) => (
          <MenuItem key={v.id} selected={v.id === current.id} onClick={() => { setAnchor(null); selection.selectVersion(String(v.id)); }}>
            {v.name}
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}

function ExecutorChips({ agent, several }: { readonly agent: PendingAgent; readonly several: boolean }): React.JSX.Element {
  return (
    <>
      <Chip size="small" variant="outlined" label={agent.cloud ? t('chat.participants.cloud', 'Cloud') : t('chat.participants.local', 'Local')} data-executor={agent.cloud ? 'cloud' : 'local'} />
      {agent.lead && several && <Chip size="small" label={t('chat.lead.badge', 'Lead')} data-testid="thread-pending-lead" />}
    </>
  );
}

interface RowProps {
  readonly agent: PendingAgent;
  readonly selection: AgentSelection;
  readonly several: boolean;
  readonly locked: boolean;
}

function PendingRow({ agent, selection, several, locked }: RowProps): React.JSX.Element {
  const others = selection.agents.filter((a) => a.id !== agent.agentId).map((a) => ({ id: a.id, label: a.name }));
  return (
    <Box data-testid="thread-pending-agent" data-agent-id={agent.agentId} sx={{ display: 'flex', flexDirection: 'column', gap: 0.25, paddingY: 0.75 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }}>
        <Typography variant="labelMedium" noWrap sx={{ flex: 1, minWidth: 0 }}>{agent.name}</Typography>
        <ExecutorChips agent={agent} several={several} />
      </Box>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, flexWrap: 'wrap' }}>
        {agent.lead ? <VersionChoice selection={selection} /> : agent.version !== '' && <Typography variant="bodySmall" color="text.secondary">{agent.version}</Typography>}
        {agent.lead && (
          <SearchPicker label={t('workspace.participants.change', 'Change')} testId="thread-lead-change" variant="text" options={others} disabled={locked} onPick={(id) => selection.selectAgent(id)} />
        )}
        {!agent.lead && !agent.cloud && !isCloudOnlyPick(selection) && selection.makeLead !== undefined && selection.conversationId === '' && (
          <Button size="small" variant="text" data-testid="thread-pending-make-lead" disabled={locked} onClick={() => selection.makeLead?.(agent.agentId)}>
            {t('chat.lead.make', 'Make lead')}
          </Button>
        )}
        {!agent.lead && (
          <Button size="small" variant="text" color="error" data-testid="thread-pending-remove" disabled={locked} onClick={() => selection.removeCompanion?.(agent.agentId)}>
            {t('chat.participants.remove', 'Remove from thread')}
          </Button>
        )}
      </Box>
    </Box>
  );
}

function AddButtons({ selection, joined, locked }: { readonly selection: AgentSelection; readonly joined: readonly ThreadAgent[]; readonly locked: boolean }): React.JSX.Element | null {
  const add = selection.addCompanion;
  if (add === undefined) return null;
  const teams = teamOptions(selection);
  return (
    <Box sx={{ display: 'flex', gap: 0.5, flexWrap: 'wrap', paddingTop: 1 }}>
      <SearchPicker label={t('workspace.floor.addLocal', 'Add an agent')} testId="thread-add-agent" icon={<AddIcon />} disabled={locked} options={addOptions(selection, joined, false)} onPick={(id) => add(id, false)} />
      <SearchPicker
        label={t('workspace.floor.addCloud', 'Add a cloud agent')}
        testId="thread-add-cloud-agent"
        icon={<CloudOutlinedIcon />}
        disabled={locked}
        options={addOptions(selection, joined, true)}
        note={t('workspace.floor.cloudNote', 'A cloud agent gets the conversation text only, never this folder.')}
        onPick={(id) => add(id, true)}
      />
      {teams.length > 0 && selection.selectTeam !== undefined && (
        <SearchPicker
          label={t('workspace.team.add', 'Add team')}
          testId="thread-add-team"
          icon={<GroupsOutlinedIcon />}
          disabled={locked}
          options={teams}
          note={t('workspace.team.note', 'Starts a new thread with the team’s agents and lead. They all work on this computer with this folder.')}
          onPick={(id) => selection.selectTeam?.(Number(id))}
        />
      )}
    </Box>
  );
}

interface ThreadAgentsPanelProps {
  readonly selection: AgentSelection;
  /** The agents the conversation holds (the rows above). */
  readonly joined: readonly ThreadAgent[];
  /** The thread is in the cloud: its agents cannot change here. */
  readonly locked: boolean;
}

export function ThreadAgentsPanel({ selection, joined, locked }: ThreadAgentsPanelProps): React.JSX.Element | null {
  if (selection.empty || selection.agentId === '') return null;
  const pending = pendingAgents(selection, joined);
  const several = pending.length + joined.length >= 2;
  return (
    <Box data-testid="thread-agents-panel" sx={(theme: Theme) => ({ display: 'flex', flexDirection: 'column', paddingX: 2, paddingY: 1, borderTop: joined.length > 0 ? `0.0625rem solid ${theme.vars.palette.divider}` : 'none' })}>
      {selection.team !== undefined && (
        <Typography variant="labelSmall" color="text.secondary" data-testid="thread-team-name">
          {t('workspace.participants.team', 'Team: {{name}}', { name: selection.team.name })}
        </Typography>
      )}
      {pending.length > 0 && joined.length > 0 && (
        <Typography variant="labelSmall" color="text.secondary">{t('workspace.participants.joining', 'Join with your next message')}</Typography>
      )}
      {pending.map((agent) => (
        <PendingRow key={agent.agentId} agent={agent} selection={selection} several={several} locked={locked} />
      ))}
      <AddButtons selection={selection} joined={joined} locked={locked} />
    </Box>
  );
}
