/**
 * "Send": make sure the conversation exists, then start the turn on the host.
 */
import { useState } from 'react';

import { describeWorkspaceError, type WorkspaceTurn } from '@/features/workspace';
import type { Workspace } from '@/shared/desktop/workspaceIpc';

import type { AgentSelection } from './useAgentSelection';
import { useEnsureConversation } from './useEnsureConversation';

export interface SendPrompt {
  canSend: boolean;
  sendError: string | null;
  send: (prompt: string, planMode: boolean) => Promise<boolean>;
}

export function useSendPrompt(workspace: Workspace, projectId: number, selection: AgentSelection, turn: WorkspaceTurn): SendPrompt {
  const ensureConversation = useEnsureConversation();
  const [sendError, setSendError] = useState<string | null>(null);
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);

  const send = async (prompt: string, planMode: boolean): Promise<boolean> => {
    if (agent === undefined || version === undefined || prompt.trim() === '' || turn.busy) return false;
    setSendError(null);
    try {
      const conversationId = await ensureConversation({
        projectId,
        conversationId: selection.conversationId,
        prompt,
        applicationId: Number(agent.id),
        applicationName: agent.name,
        versionId: version.id,
        agentType: version.agentType,
      });
      const started = await turn.start({
        workspace_id: workspace.id,
        project_id: projectId,
        conversation_id: conversationId,
        application_id: Number(agent.id),
        version_id: version.id,
        prompt,
        plan_mode: planMode,
      });
      // A refused start (its reason is the turn's startError) keeps the
      // prompt and the person's conversation choice as they were.
      if (!started) return false;
      selection.selectConversation(conversationId);
      return true;
    } catch (error) {
      setSendError(describeWorkspaceError(error));
      return false;
    }
  };

  return { canSend: agent !== undefined && version !== undefined && !turn.busy, sendError, send };
}
