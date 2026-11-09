import { useQueryClient } from '@tanstack/react-query';
import { router, useLocalSearchParams } from 'expo-router';
import { useRef, useState } from 'react';

import {
  StartPublicChatError,
  startConversationWithPublicAgent,
  startPublicChatErrorText,
  useCachedPublicAgent,
  usePublicAgentDetail,
} from '@/api/queries/discover';
import { AGENT_MEMORY_CONTRACT } from '@/api/queries/agentMemory';
import { queryKeys } from '@/api/queries/keys';
import { speaksContract } from '@/api/discovery';
import { StatusScreen } from '@/components/StatusScreen';
import { PublicAgentView } from '@/features/discover/PublicAgentView';
import { useDiscoverAvailable } from '@/features/discover/useDiscoverAvailable';
import { handOff } from '@/features/intents/intentHandoff';
import { MAX_PREFILL } from '@/features/intents/intentRouter';
import { useProjectLabel } from '@/features/projects/ProjectChip';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { Button } from '@/ui';

/**
 * One public agent, in a sheet over Discover: what it is, and a chat with it in the ACTIVE project
 * (createConversation there, then the agent as a participant at its
 * published version). A conversation starter opens the new chat with that
 * text in the composer; it is never sent by itself.
 */
export default function PublicAgentScreen() {
  const queryClient = useQueryClient();
  const available = useDiscoverAvailable();
  const { projectId } = useProjectSelection();
  const projectLabel = useProjectLabel();
  const { id } = useLocalSearchParams<{ id: string }>();
  const agentId = typeof id === 'string' && /^\d{1,15}$/.test(id) ? Number(id) : null;
  const agent = useCachedPublicAgent(projectId, agentId);
  const detail = usePublicAgentDetail(projectId, agent);
  const [busy, setBusy] = useState<string | null>(null);
  // Set synchronously: a second touch in the same frame sees it before `busy` (or the
  // button's disabled state) has rendered, so one tap creates one conversation.
  const starting = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const workspace = useWorkspaceInfo();
  const memoryAvailable = speaksContract(workspace?.discovery, AGENT_MEMORY_CONTRACT.major, AGENT_MEMORY_CONTRACT.minor);

  if (!available) return <StatusScreen title="Discover isn't available" />;
  if (projectId === null) return <StatusScreen title="No project selected" />;
  if (agent === null) {
    return (
      <StatusScreen title="Agent not found" body="It is no longer in the list. Go back and search again.">
        <Button title="Back to Discover" variant="tonal" block={false} onPress={() => router.back()} />
      </StatusScreen>
    );
  }

  const start = async (key: string, prefill: string | null) => {
    if (starting.current) return;
    starting.current = true;
    setBusy(key);
    setError(null);
    try {
      const created = await startConversationWithPublicAgent(projectId, agent);
      void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
      const params: { id: string; intent?: string } = { id: String(created.id) };
      if (prefill !== null) params.intent = handOff({ agent: null, prefill: prefill.slice(0, MAX_PREFILL) });
      router.replace({ pathname: '/c/[id]', params });
    } catch (e) {
      // A conversation that could not be deleted again shows in the list now, not at some later refetch.
      if (e instanceof StartPublicChatError && e.leftover) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
      }
      setError(startPublicChatErrorText(e));
      starting.current = false;
      setBusy(null);
    }
  };

  return (
    <PublicAgentView
      agent={agent}
      projectId={projectId}
      projectLabel={projectLabel}
      welcome={detail.data?.welcomeMessage ?? null}
      starters={detail.data?.starters ?? []}
      busy={busy}
      error={error}
      onStart={() => void start('start', null)}
      onStarter={(index, text) => void start(`starter:${index}`, text)}
      // The agent's memory is kept in the project its chats run in: the active one.
      onMemory={
        memoryAvailable ? () => router.push({ pathname: '/agent-memory/[id]', params: { id: String(agent.id), name: agent.name } }) : undefined
      }
    />
  );
}
