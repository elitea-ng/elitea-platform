/**
 * The skills pinned to a local thread (IPC.md "Thread history", ADR-0050 amendment 1, A1.5; #2183), kept beside
 * `workspaceIpc.ts`. A pin lives in the desktop host's thread store, not in the UI: it is a project skill's id, and
 * the host reads the text at every turn under the person's own token. Wire shapes are the host's (snake_case).
 */

/** One project skill pinned to a thread. */
export interface ThreadSkillPin {
  projectId: number;
  skillId: number;
}

/** The thread-skill half of `WorkspaceIpc`. */
export interface ThreadSkillCommands {
  /** `thread_skills_list`: the thread's pins, oldest first (none when it has none). */
  threadSkills(workspaceId: string, conversationId: string): Promise<ThreadSkillPin[]>;
  /**
   * `thread_skill_pin`: pin a project skill the person can read to the thread. Rejects `skill_unknown` (not readable)
   * and `thread_skills_full` (a thread holds five); pinning one twice changes nothing.
   */
  pinThreadSkill(workspaceId: string, conversationId: string, pin: ThreadSkillPin): Promise<void>;
  /** `thread_skill_unpin`: resolves whether the skill was pinned. */
  unpinThreadSkill(workspaceId: string, conversationId: string, skillId: number): Promise<boolean>;
}

type Invoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

interface WirePin {
  project_id: number;
  skill_id: number;
}

export function threadSkillCommands(invoke: Invoke): ThreadSkillCommands {
  return {
    threadSkills: async (workspaceId, conversationId) =>
      (await invoke<WirePin[]>('thread_skills_list', { workspace_id: workspaceId, conversation_id: conversationId })).map((pin) => ({
        projectId: pin.project_id,
        skillId: pin.skill_id,
      })),
    pinThreadSkill: (workspaceId, conversationId, pin) =>
      invoke<void>('thread_skill_pin', { workspace_id: workspaceId, conversation_id: conversationId, project_id: pin.projectId, skill_id: pin.skillId }),
    unpinThreadSkill: (workspaceId, conversationId, skillId) =>
      invoke<boolean>('thread_skill_unpin', { workspace_id: workspaceId, conversation_id: conversationId, skill_id: skillId }),
  };
}
