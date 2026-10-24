/**
 * The open thread's name (`threadNames`): read back from the server when the
 * thread opens, and generated once a turn of it is stored (the done event
 * names the conversation, also for a thread that had none before).
 */
import { threadNames, type WorkspaceTurn } from '@/features/workspace';

type TurnDone = WorkspaceTurn['view']['done'];

export function useThreadName(workspaceId: string, projectId: number, conversationId: string, done: TurnDone): void {
  threadNames.useServerName({ workspaceId, projectId, conversationId });
  const stored = done?.committed === true;
  threadNames.useNaming({ workspaceId, projectId, conversationId: done?.conversationId ?? conversationId }, stored);
}
