/**
 * Which agent (and version) and which conversation the session talks to.
 * Only the BOUND project's agents are offered; a pipeline is not an agent
 * here, so the listing is filtered to `classic`.
 *
 * The agent and version last used in a workspace are remembered per
 * workspace and project (`el.desktop.workspace.<id>.agent`) and picked
 * again when the session opens; otherwise the first agent is.
 */
import { useEffect, useMemo, useState } from 'react';

import { useGetApplication, useListApplications } from '@/shared/api/generated/applications/applications';
import { useListConversations } from '@/shared/api/generated/chat/chat';
import type { Application, ApplicationDetail } from '@/shared/api/generated/model';
import { unwrapBody, unwrapList } from '@/shared/api/unwrap';
import { createStorage } from '@/shared/lib/storage';

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

export interface AgentSelection {
  agents: readonly Application[];
  versions: readonly VersionChoice[];
  conversations: readonly ConversationChoice[];
  agentId: string;
  versionId: string;
  /** '' means "start a new conversation on first send". */
  conversationId: string;
  loading: boolean;
  /** The bound project's agent list was answered and holds none. */
  empty: boolean;
  selectAgent(id: string): void;
  selectVersion(id: string): void;
  selectConversation(id: string): void;
}

export function useAgentSelection(workspaceId: string, projectId: number | null): AgentSelection {
  const project = projectId === null ? '' : String(projectId);
  const enabled = projectId !== null;
  const [agentId, setAgentId] = useState('');
  const [versionId, setVersionId] = useState('');
  const [conversationId, setConversationId] = useState('');

  const agentsQuery = useListApplications(project, { agents_type: 'classic', limit: 100 }, { query: { enabled } });
  const agents = useMemo(() => unwrapList<Application>(agentsQuery.data, 'workspace.agents'), [agentsQuery.data]);

  const detailQuery = useGetApplication(project, Number(agentId), { query: { enabled: enabled && agentId !== '' } });
  const versions = useMemo<VersionChoice[]>(() => {
    const detail = unwrapBody(detailQuery.data) as ApplicationDetail | undefined;
    return (detail?.versions ?? []).map((v) => ({ id: Number(v.id), name: v.name, agentType: v.agent_type }));
  }, [detailQuery.data]);

  const conversationsQuery = useListConversations(project, { mine: true, limit: 50, offset: 0 }, { query: { enabled } });
  const conversations = useMemo(
    () => unwrapList<ConversationChoice>(conversationsQuery.data, 'workspace.conversations'),
    [conversationsQuery.data],
  );

  // Nothing picked yet: the agent last used here, else the first one.
  useEffect(() => {
    if (agentId !== '' || agents.length === 0) return;
    const last = readLastUsed(workspaceId, projectId);
    const remembered = last !== null && agents.some((a) => a.id === last.agentId) ? last.agentId : undefined;
    setAgentId(remembered ?? agents[0]?.id ?? '');
  }, [agents, agentId, workspaceId, projectId]);

  // A different agent has different versions: drop a stale pick, default to
  // the one last used with this agent, else the first offered.
  useEffect(() => {
    if (versions.length === 0) {
      setVersionId('');
      return;
    }
    if (versions.some((v) => String(v.id) === versionId)) return;
    const last = readLastUsed(workspaceId, projectId);
    const remembered = last?.agentId === agentId && versions.some((v) => String(v.id) === last.versionId) ? last.versionId : undefined;
    setVersionId(remembered ?? String(versions[0]?.id ?? ''));
  }, [versions, versionId, agentId, workspaceId, projectId]);

  // Remember a complete pick for the next time this workspace opens.
  useEffect(() => {
    if (projectId === null || agentId === '' || versionId === '' || !versions.some((v) => String(v.id) === versionId)) return;
    writeLastUsed(workspaceId, { projectId, agentId, versionId });
  }, [workspaceId, projectId, agentId, versionId, versions]);

  return {
    agents,
    versions,
    conversations,
    agentId,
    versionId,
    conversationId,
    loading: agentsQuery.isPending && enabled,
    empty: enabled && agentsQuery.isSuccess && agents.length === 0,
    // A conversation holds its agent on one version: another agent or
    // version starts a new conversation (else the next send is refused with
    // agent_not_in_conversation / agent_version_mismatch).
    selectAgent: (id) => {
      setAgentId(id);
      setVersionId('');
      setConversationId('');
    },
    selectVersion: (id) => {
      setVersionId(id);
      setConversationId('');
    },
    selectConversation: setConversationId,
  };
}
