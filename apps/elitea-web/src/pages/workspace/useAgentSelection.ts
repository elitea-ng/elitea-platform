/**
 * Which agent (and version) and which conversation the session talks to.
 * Only the BOUND project's agents are offered; a pipeline is not an agent
 * here, so the listing is filtered to `classic`.
 */
import { useEffect, useMemo, useState } from 'react';

import { useGetApplication, useListApplications } from '@/shared/api/generated/applications/applications';
import { useListConversations } from '@/shared/api/generated/chat/chat';
import type { Application, ApplicationDetail } from '@/shared/api/generated/model';
import { unwrapBody, unwrapList } from '@/shared/api/unwrap';

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
  selectAgent(id: string): void;
  selectVersion(id: string): void;
  selectConversation(id: string): void;
}

export function useAgentSelection(projectId: number | null): AgentSelection {
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

  // A different agent has different versions: drop a stale pick, default to the first offered.
  useEffect(() => {
    if (versions.length === 0) setVersionId('');
    else if (!versions.some((v) => String(v.id) === versionId)) setVersionId(String(versions[0]?.id ?? ''));
  }, [versions, versionId]);

  return {
    agents,
    versions,
    conversations,
    agentId,
    versionId,
    conversationId,
    loading: agentsQuery.isPending && enabled,
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
