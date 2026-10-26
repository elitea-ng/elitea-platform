import { router } from 'expo-router';

import { speaksContract } from '@/api/discovery';
import { StatusScreen } from '@/components/StatusScreen';
import { AgentBuilderView } from '@/features/agentBuilder/AgentBuilderView';
import { useAgentBuilder } from '@/features/agentBuilder/useAgentBuilder';
import { useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Create an agent (ADR-0037, client contract 1.28): a modal over New chat.
 * Saved, it opens a chat with the new agent in place of this screen.
 */
export default function CreateAgentScreen() {
  const projectId = useSession((s) => s.projectId);
  const workspace = useWorkspaceInfo();
  const builder = useAgentBuilder(projectId);
  if (projectId === null) return <StatusScreen title="No project selected" />;
  if (!speaksContract(workspace?.discovery, 1, 28)) return <StatusScreen title="This workspace can't create agents from the app yet" />;
  return <AgentBuilderView builder={builder} onSaved={(id) => router.replace({ pathname: '/c/[id]', params: { id: String(id) } })} />;
}
