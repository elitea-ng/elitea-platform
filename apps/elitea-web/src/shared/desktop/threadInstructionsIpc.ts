/**
 * A local thread's project instructions (IPC.md "Project instructions", issue 3130), kept beside `workspaceIpc.ts`.
 * The folder's AGENTS.md (and SOUL.md) are read by the desktop host at every turn start; whether a thread applies
 * them ("Respect project instructions") is the thread's own setting, kept in the host's thread store, on by default. A change
 * applies from the thread's next turn, for every agent of the thread. Wire shapes are the host's (snake_case).
 */

/** The `status` `running` fields that name the project instruction files a turn found. */
export interface ProjectInstructionsFields {
  /** The files the folder held for the turn (the root's first, then the nested ones of the referenced paths). */
  project_instructions?: string[];
  /** Whether the turn applied them: false when the thread does not respect AGENTS.md. Present with the files. */
  project_instructions_applied?: boolean;
}

/** `thread_instructions_status`: the files at the folder's root and whether the thread respects them. */
interface ThreadInstructionsStatus {
  files: string[];
  respect: boolean;
}

/** The project-instructions half of the thread commands. */
export interface ThreadInstructionsCommands {
  /** `thread_instructions_status`; `conversationId` `undefined` for a thread not created yet (reads as on). */
  threadInstructionsStatus(workspaceId: string, conversationId: string | undefined): Promise<ThreadInstructionsStatus>;
  /** `thread_respect_agents_md_set`: kept by the host; the thread's next turn follows it. */
  setThreadRespectAgentsMd(workspaceId: string, conversationId: string, respect: boolean): Promise<void>;
}

type Invoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

export function threadInstructionsCommands(invoke: Invoke): ThreadInstructionsCommands {
  return {
    threadInstructionsStatus: (workspaceId, conversationId) =>
      invoke<ThreadInstructionsStatus>('thread_instructions_status', {
        workspace_id: workspaceId,
        ...(conversationId === undefined ? {} : { conversation_id: conversationId }),
      }),
    setThreadRespectAgentsMd: (workspaceId, conversationId, respect) =>
      invoke<void>('thread_respect_agents_md_set', { workspace_id: workspaceId, conversation_id: conversationId, respect }),
  };
}
