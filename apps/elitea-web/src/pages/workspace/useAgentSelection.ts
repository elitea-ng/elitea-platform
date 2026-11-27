/**
 * Which agent (and version) and which conversation the session talks to.
 * Only the BOUND project's agents are offered: its `classic` agents, then its
 * pipelines (#1239 A3). A pipeline does not run on this computer: picking one
 * makes the composer run it in the cloud (`isCloudOnlyPick`, `useRunInCloud`).
 *
 * The agent and version last used in a workspace are remembered per
 * workspace and project (`el.desktop.workspace.<id>.agent`) and picked
 * again when the session opens; otherwise the folder's default Local agent
 * is, when the project has it (`defaultAgentId`: the Local documents agent
 * for a folder of documents or a mixed one, the Local coding agent for a git
 * or code folder), else the first agent.
 *
 * Several agents in one thread (ADR-0050 decision 7f): besides the composer's
 * agent, the person may add more of the project's agents to the thread
 * (`companions`), each running on this computer with the workspace or in the
 * cloud with the conversation's text only. They join the conversation on the
 * next send; a message names the ones it is for with "@Name".
 *
 * A saved team (ADR-0050 decision 8, #3053): picking one of the project's teams starts a new thread run by the team
 * (`workTeams.ts`): the composer's agent is its lead (or its first member), on the team's version, and the other
 * members are the companions, each running on this computer. The first send writes the lead as the thread's floor.
 *
 * A worktree's new thread starts from the pick made in its folder's new thread (`seed`, #3128): the first message
 * written there with "New worktree" moves here with the agent, version, companions and team chosen for it, so the
 * thread's first turn runs them, not the folder's default agent.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { getApplication, useGetApplication, useListApplications } from '@/shared/api/generated/applications/applications';
import { useListConversations } from '@/shared/api/generated/chat/chat';
import type { Application, ApplicationDetail } from '@/shared/api/generated/model';
import { unwrapBody, unwrapList } from '@/shared/api/unwrap';
import type { FolderKind } from '@/shared/desktop/workspace';
import { createStorage } from '@/shared/lib/storage';
import { useAgentTeamList, useTeamAbilities } from '@/features/agent-teams';

import { defaultLocalAgent, localAgentName } from './useLocalCodingAgent';
import { teamChoice, type TeamChoice, type WorkTeam } from './workTeams';

interface LastUsed {
  projectId: number;
  agentId: string;
  versionId: string;
}

const lastUsedKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.agent`;

function isLastUsed(raw: unknown): LastUsed | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined;
  const { projectId, agentId, versionId } = raw as Record<string, unknown>;
  return typeof projectId === 'number' && typeof agentId === 'string' && typeof versionId === 'string' ? { projectId, agentId, versionId } : undefined;
}

function readLastUsed(workspaceId: string, projectId: number | null): LastUsed | null {
  try {
    // `getJSON` treats a validator's `undefined` as absent (`null`).
    const stored = createStorage('local').getJSON<LastUsed>(lastUsedKey(workspaceId), isLastUsed as (raw: unknown) => LastUsed);
    return stored !== null && stored.projectId === projectId ? stored : null;
  } catch {
    return null;
  }
}

function writeLastUsed(workspaceId: string, value: LastUsed): void {
  try {
    createStorage('local').setJSON(lastUsedKey(workspaceId), value);
  } catch {
    // Storage may be unavailable: the pick is then not remembered.
  }
}

/** The slice of a listed conversation the picker shows. */
interface ConversationChoice {
  readonly id: string | number;
  readonly name: string;
}

interface VersionChoice {
  id: number;
  name: string;
  agentType: string;
}

/** Another agent of the thread (ADR-0050 decision 7f), on the version it was added with. */
interface ThreadCompanion {
  agentId: string;
  name: string;
  versionId: number;
  agentType: string;
  /** Runs in the cloud with the conversation's text only (participant `executor: cloud`), never the workspace. */
  cloud: boolean;
  /** The version's name, when known (an agent added from the sidebar; a team's member names none). */
  versionName?: string;
}

/** The pick a new thread hands to the worktree's thread its first message moves to (#3128). */
export interface SelectionSeed {
  readonly projectId: number;
  readonly agentId: string;
  readonly versionId: string;
  readonly companions: readonly ThreadCompanion[];
  readonly team?: WorkTeam;
}

export interface AgentSelection {
  agents: readonly Application[];
  versions: readonly VersionChoice[];
  conversations: readonly ConversationChoice[];
  agentId: string;
  versionId: string;
  /** '' means "start a new conversation on first send". */
  conversationId: string;
  /** The ids of `agents` listed as pipelines: they run only in the cloud. Absent = none. */
  pipelineIds?: ReadonlySet<string>;
  loading: boolean;
  /** The bound project's agent list was answered and holds none. */
  empty: boolean;
  selectAgent(id: string): void;
  selectVersion(id: string): void;
  selectConversation(id: string): void;
  /** The thread's other agents, added by the person; absent = none. */
  companions?: readonly ThreadCompanion[];
  /** Add `id` to the thread, on its first version; `cloud`: it runs in the cloud. Absent = not offered. */
  addCompanion?: (id: string, cloud: boolean) => void;
  removeCompanion?: (id: string) => void;
  /**
   * Before the thread's first send (#3081): the companion `id` becomes the composer's agent (the one a message that
   * names no agent goes to, the team's lead), and the composer's agent one of the companions, on this computer.
   */
  makeLead?: (id: string) => void;
  /** The project's saved teams, as the picker lists them; absent = none offered. */
  teams?: readonly TeamChoice[];
  /** The team the thread is run by (a new thread, or the one its first send created); absent = none. */
  team?: WorkTeam;
  /** Run a new thread by the team `id` (a runnable one of `teams`). */
  selectTeam?: (id: number) => void;
  /** A send created thread `id` for the new thread: selecting it keeps the team and the companions. */
  markCreated?: (id: string) => void;
}

const NO_COMPANIONS: readonly ThreadCompanion[] = [];
const NO_TEAMS: readonly TeamChoice[] = [];

/** The selection's pick as a seed, or `undefined` before an agent and version are picked. */
export function selectionSeed(selection: Pick<AgentSelection, 'agentId' | 'versionId' | 'companions' | 'team'>, projectId: number): SelectionSeed | undefined {
  if (selection.agentId === '' || selection.versionId === '') return undefined;
  const { agentId, versionId, team } = selection;
  return { projectId, agentId, versionId, companions: selection.companions ?? NO_COMPANIONS, ...(team === undefined ? {} : { team }) };
}

/** The companion came with the team (one of its other members). */
function fromTeam(team: WorkTeam | undefined, companion: ThreadCompanion): boolean {
  return team?.others.some((member) => String(member.applicationId) === companion.agentId) === true;
}

/**
 * Selecting `id` leaves the thread the companions (and the team) were picked for: none, another existing thread, or,
 * from a new thread, any thread but the one its first send created (`created`).
 */
function leavesThread(id: string, current: string, created: string): boolean {
  return id === '' || (current !== '' && id !== current) || (current === '' && id !== created);
}

/** The version the team pinned its lead to, when `agentId` is that lead and the version is offered; else `undefined`. */
function teamVersion(team: WorkTeam | undefined, agentId: string, versions: readonly VersionChoice[]): string | undefined {
  if (team === undefined || String(team.primary.applicationId) !== agentId) return undefined;
  const pinned = String(team.primary.versionId);
  return versions.some((v) => String(v.id) === pinned) ? pinned : undefined;
}

/** A team's other members as the thread's companions: on the team's version, on this computer. */
function teamCompanions(team: WorkTeam): ThreadCompanion[] {
  return team.others.map((member) => ({ agentId: String(member.applicationId), name: member.name, versionId: member.versionId, agentType: '', cloud: false }));
}

/** `team` led by its member `next` (#3081): the former lead joins the others, first. */
function swapLead(team: WorkTeam, next: ThreadCompanion): WorkTeam {
  const member = team.others.find((m) => String(m.applicationId) === next.agentId) ?? { applicationId: Number(next.agentId), versionId: next.versionId, name: next.name };
  return { ...team, primary: member, others: [team.primary, ...team.others.filter((m) => m !== member)] };
}

/** The version `wanted` names for `agentId`, when it is offered; else `undefined`. */
function wantedVersion(wanted: { agentId: string; versionId: string } | null, agentId: string, versions: readonly VersionChoice[]): string | undefined {
  if (wanted?.agentId !== agentId) return undefined;
  return versions.some((v) => String(v.id) === wanted.versionId) ? wanted.versionId : undefined;
}

/** The first version of an agent, read when the person adds it to a thread. */
async function firstVersion(projectId: string, agentId: string): Promise<VersionChoice | undefined> {
  const detail = unwrapBody(await getApplication(projectId, Number(agentId))) as ApplicationDetail | undefined;
  const version = detail?.versions[0];
  return version === undefined ? undefined : { id: Number(version.id), name: version.name, agentType: version.agent_type };
}

/** The pick runs only in the cloud: a pipeline (listed as one, or its version is one). */
export function isCloudOnlyPick(selection: AgentSelection): boolean {
  if (selection.agentId === '') return false;
  if (selection.pipelineIds?.has(selection.agentId) === true) return true;
  return selection.versions.find((v) => String(v.id) === selection.versionId)?.agentType === 'pipeline';
}

const EMPTY_IDS: ReadonlySet<string> = new Set();

const NO_PICK: SelectionSeed = { projectId: -1, agentId: '', versionId: '', companions: NO_COMPANIONS };

/** What the session starts with: the seed, when it is of this project; else nothing picked. */
function initialPick(seed: SelectionSeed | undefined, projectId: number | null): { pick: SelectionSeed; seeded: boolean } {
  return seed !== undefined && seed.projectId === projectId ? { pick: seed, seeded: true } : { pick: NO_PICK, seeded: false };
}

/** The agent a session picks when nothing is remembered: the folder's default Local agent when the project has it, else the first. */
export function defaultAgentId(agents: readonly Pick<Application, 'id' | 'name'>[], kind: FolderKind | undefined): string {
  const name = localAgentName(defaultLocalAgent(kind));
  return agents.find((agent) => agent.name === name)?.id ?? agents[0]?.id ?? '';
}

export function useAgentSelection(workspaceId: string, projectId: number | null, seed?: SelectionSeed, folderKind?: FolderKind): AgentSelection {
  const project = projectId === null ? '' : String(projectId);
  const enabled = projectId !== null;
  // A seed of another project is not this folder's pick.
  const [{ pick: start, seeded }] = useState(() => initialPick(seed, projectId));
  const [agentId, setAgentId] = useState(start.agentId);
  const [versionId, setVersionId] = useState(start.versionId);
  const [conversationId, setConversationId] = useState('');
  const [companions, setCompanions] = useState<readonly ThreadCompanion[]>(start.companions);
  const [team, setTeam] = useState<WorkTeam | undefined>(start.team);
  // The thread the new thread's first send created (`markCreated`).
  const created = useRef('');
  // The version a member made lead was added with (`makeLead`), or the seed's: picked once the agent's versions are read.
  const wanted = useRef<{ agentId: string; versionId: string } | null>(seeded ? { agentId: start.agentId, versionId: start.versionId } : null);

  const agentsQuery = useListApplications(project, { agents_type: 'classic', limit: 100 }, { query: { enabled } });
  const pipelinesQuery = useListApplications(project, { agents_type: 'pipeline', limit: 100 }, { query: { enabled } });
  const classic = useMemo(() => unwrapList<Application>(agentsQuery.data, 'workspace.agents'), [agentsQuery.data]);
  const pipelines = useMemo(() => unwrapList<Application>(pipelinesQuery.data, 'workspace.pipelines'), [pipelinesQuery.data]);
  // The agents, then the pipelines not already listed (an application is one or the other).
  const { agents, pipelineIds } = useMemo(() => {
    const known = new Set(classic.map((a) => a.id));
    const extra = pipelines.filter((p) => !known.has(p.id));
    return { agents: [...classic, ...extra], pipelineIds: extra.length === 0 ? EMPTY_IDS : new Set(extra.map((p) => p.id)) };
  }, [classic, pipelines]);

  const detailQuery = useGetApplication(project, Number(agentId), { query: { enabled: enabled && agentId !== '' } });
  const versions = useMemo<VersionChoice[]>(() => {
    const detail = unwrapBody(detailQuery.data) as ApplicationDetail | undefined;
    return (detail?.versions ?? []).map((v) => ({ id: Number(v.id), name: v.name, agentType: v.agent_type }));
  }, [detailQuery.data]);

  const { canList: canListTeams } = useTeamAbilities(enabled ? project : undefined);
  const teamList = useAgentTeamList(enabled ? project : undefined, '', canListTeams);
  const teams = useMemo(
    () => (projectId === null || teamList.rows.length === 0 ? NO_TEAMS : teamList.rows.map((row) => teamChoice(row, projectId, agents, pipelineIds))),
    [teamList.rows, projectId, agents, pipelineIds],
  );

  const conversationsQuery = useListConversations(project, { mine: true, limit: 50, offset: 0 }, { query: { enabled } });
  const conversations = useMemo(
    () => unwrapList<ConversationChoice>(conversationsQuery.data, 'workspace.conversations'),
    [conversationsQuery.data],
  );

  // Nothing picked yet: the agent last used here, else the folder's default one.
  useEffect(() => {
    if (agentId !== '' || agents.length === 0) return;
    const last = readLastUsed(workspaceId, projectId);
    const remembered = last !== null && agents.some((a) => a.id === last.agentId) ? last.agentId : undefined;
    setAgentId(remembered ?? defaultAgentId(agents, folderKind));
  }, [agents, agentId, workspaceId, projectId, folderKind]);

  // A different agent has different versions: drop a stale pick, default to
  // the one last used with this agent, else the first offered.
  useEffect(() => {
    if (versions.length === 0) {
      setVersionId('');
      return;
    }
    if (versions.some((v) => String(v.id) === versionId)) return;
    const pinned = teamVersion(team, agentId, versions) ?? wantedVersion(wanted.current, agentId, versions);
    if (pinned !== undefined) {
      wanted.current = null;
      setVersionId(pinned);
      return;
    }
    const last = readLastUsed(workspaceId, projectId);
    const remembered = last?.agentId === agentId && versions.some((v) => String(v.id) === last.versionId) ? last.versionId : undefined;
    setVersionId(remembered ?? String(versions[0]?.id ?? ''));
  }, [versions, versionId, agentId, workspaceId, projectId, team]);

  // Remember a complete pick for the next time this workspace opens.
  useEffect(() => {
    if (projectId === null || agentId === '' || versionId === '' || !versions.some((v) => String(v.id) === versionId)) return;
    writeLastUsed(workspaceId, { projectId, agentId, versionId });
  }, [workspaceId, projectId, agentId, versionId, versions]);

  const addCompanion = useCallback(
    (id: string, cloud: boolean) => {
      const agent = agents.find((a) => a.id === id);
      if (agent === undefined || project === '') return;
      void firstVersion(project, id).then((version) => {
        if (version === undefined) return;
        const companion: ThreadCompanion = { agentId: id, name: agent.name, versionId: version.id, agentType: version.agentType, cloud, versionName: version.name };
        setCompanions((current) => [...current.filter((c) => c.agentId !== id), companion]);
      });
    },
    [agents, project],
  );
  const removeCompanion = useCallback((id: string) => setCompanions((current) => current.filter((c) => c.agentId !== id)), []);
  const makeLead = useCallback(
    (id: string) => {
      const next = companions.find((c) => c.agentId === id);
      const lead = agents.find((a) => a.id === agentId);
      const version = versions.find((v) => String(v.id) === versionId);
      // The composer's agent runs on this computer: a cloud agent is not made lead, nor is a cloud-only lead (a
      // pipeline) moved here; and the lead's version must be known to keep it.
      if (next === undefined || next.cloud || lead === undefined || version === undefined || conversationId !== '') return;
      if (pipelineIds.has(agentId) || version.agentType === 'pipeline') return;
      const former: ThreadCompanion = { agentId, name: lead.name, versionId: version.id, agentType: version.agentType, cloud: false, versionName: version.name };
      wanted.current = { agentId: id, versionId: String(next.versionId) };
      setCompanions((current) => [former, ...current.filter((c) => c.agentId !== id)]);
      setTeam((current) => (current === undefined ? undefined : swapLead(current, next)));
      setAgentId(id);
      setVersionId(String(next.versionId));
    },
    [companions, agents, agentId, versionId, versions, conversationId, pipelineIds],
  );
  // A team always starts a new thread (chat cannot add a team to a conversation either).
  const selectTeam = useCallback(
    (id: number) => {
      const choice = teams.find((row) => row.id === id);
      if (choice === undefined || !('team' in choice)) return;
      setTeam(choice.team);
      setAgentId(String(choice.team.primary.applicationId));
      setVersionId(String(choice.team.primary.versionId));
      setConversationId('');
      setCompanions(teamCompanions(choice.team));
    },
    [teams],
  );

  return {
    agents,
    versions,
    conversations,
    agentId,
    versionId,
    conversationId,
    pipelineIds,
    loading: agentsQuery.isPending && enabled,
    // Empty once both lists answered with nothing (a failed pipeline list counts as none).
    empty: enabled && agentsQuery.isSuccess && !pipelinesQuery.isPending && agents.length === 0,
    // A conversation holds its agent on one version: another agent or
    // version starts a new conversation (else the next send is refused with
    // agent_not_in_conversation / agent_version_mismatch).
    selectAgent: (id) => {
      setAgentId(id);
      setVersionId('');
      setConversationId('');
      // Another agent: the thread is no longer the team's, and the team's members go with it.
      setCompanions((current) => current.filter((c) => c.agentId !== id && !fromTeam(team, c)));
      setTeam(undefined);
    },
    // Another version of the lead is not the saved team any more: the agents stay, the team's name and lead go.
    selectVersion: (id) => {
      setVersionId(id);
      setConversationId('');
      setTeam(undefined);
    },
    // Another thread has its own agents: the companions (and the team) stay only for the thread a first send just created.
    selectConversation: (id) => {
      if (leavesThread(id, conversationId, created.current)) {
        setCompanions(NO_COMPANIONS);
        setTeam(undefined);
      }
      if (id === '') created.current = '';
      setConversationId(id);
    },
    markCreated: (id) => {
      created.current = id;
    },
    companions,
    addCompanion,
    removeCompanion,
    makeLead,
    teams,
    ...(team === undefined ? {} : { team }),
    selectTeam,
  };
}
