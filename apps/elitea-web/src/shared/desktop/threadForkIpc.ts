/** The fork commands of the host (IPC.md "Thread history"), kept beside `workspaceIpc.ts`. */

/** What `thread_history_fork` copies (IPC.md "Thread history"). */
export interface ThreadForkRequest {
  workspaceId: string;
  sourceConversationId: string;
  targetConversationId: string;
  targetConversationUuid: string;
  /** The fork point's turn: the turns up to and including it are copied. Absent: every turn. */
  upToTurnId?: string;
  /**
   * The fork answer's `forked_messages` (source message UUID → the copy's): the copied turns' `done`
   * events name the new conversation's messages, so they can be fork points in turn.
   */
  messageIds?: Readonly<Record<string, string>>;
}

/** The fork half of `WorkspaceIpc`. */
export interface ThreadForkCommands {
  /**
   * The local half of a fork (`thread_history_fork`): copy a thread's recorded turns, up to and including
   * `upToTurnId` (all when absent), into the conversation the server fork made, in the same folder.
   * Resolves with how many turns were copied (0 when this machine recorded none).
   */
  forkThreadHistory(request: ThreadForkRequest): Promise<number>;
  /**
   * `checkpoint_after`: the later turn whose checkpoint is the folder as `turnId` left it (restore to before
   * it, with `confirmOlder`), or `null` when no later turn changed the folder.
   */
  restorePointAfter(turnId: string): Promise<string | null>;
}

type Invoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

export function threadForkCommands(invoke: Invoke): ThreadForkCommands {
  return {
    forkThreadHistory: async (request) =>
      (
        await invoke<{ copied: number }>('thread_history_fork', {
          workspace_id: request.workspaceId,
          source_conversation_id: request.sourceConversationId,
          target_conversation_id: request.targetConversationId,
          target_conversation_uuid: request.targetConversationUuid,
          ...(request.upToTurnId === undefined ? {} : { up_to_turn_id: request.upToTurnId }),
          ...(request.messageIds === undefined ? {} : { message_ids: request.messageIds }),
        })
      ).copied,
    restorePointAfter: async (turnId) => (await invoke<{ turn_id: string | null }>('checkpoint_after', { turn_id: turnId })).turn_id,
  };
}
