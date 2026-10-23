/** The request of `thread_history_fork` (IPC.md "Thread history"), kept beside `workspaceIpc.ts`. */

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
