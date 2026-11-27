/**
 * Desktop "Local work" conversations (ADR-0029): an agent ran each turn on
 * the person's computer over a local folder and only committed the turn to
 * the server. The desktop marks the conversation it creates with this
 * `source` (the server's `conversations.LocalWorkSource`).
 *
 * The web keeps them apart from ordinary chats: the Chats list shows them only
 * under its "Local work" filter, and opening one is read-only — the server
 * refuses to continue it (409 `local_work_thread`) because it cannot see the
 * folder. The folder's name, when the desktop recorded it, is in
 * `meta.local_work.folder_name`.
 */
export const LOCAL_WORK_SOURCE = 'local_work';

/** True for a conversation (or list row) the desktop's Local work owns. */
export function isLocalWorkConversation(conversation: { readonly source?: unknown } | undefined): boolean {
  return conversation?.source === LOCAL_WORK_SOURCE;
}

/** The conversation `meta` the desktop writes on create: only the folder's name, never its path. */
export function localWorkMeta(folderName: string): { local_work: { folder_name: string } } {
  return { local_work: { folder_name: folderName } };
}

/** The folder's name from a Local work conversation's `meta`, or undefined when it was not recorded. */
export function localWorkFolderName(meta: Readonly<Record<string, unknown>> | undefined): string | undefined {
  const localWork = meta?.['local_work'];
  if (typeof localWork !== 'object' || localWork === null) return undefined;
  const name = (localWork as Record<string, unknown>)['folder_name'];
  return typeof name === 'string' && name.trim() !== '' ? name : undefined;
}

/**
 * Where a Local work thread runs once it can be handed to the cloud
 * (ADR-0034): the conversation response's `workspace_executor`. Absent means
 * `local`.
 */
export type WorkspaceExecutorName = 'local' | 'cloud' | 'returning';

export function workspaceExecutorOf(conversation: Readonly<Record<string, unknown>> | undefined): WorkspaceExecutorName {
  const value = conversation?.workspace_executor;
  return value === 'cloud' || value === 'returning' ? value : 'local';
}

/**
 * The answer's turn ran a command outside the sandbox (ADR-0061 decision 5: Main stamps
 * `meta.local_work.ran_outside_sandbox: true` from the commit's trust record). Absent, as before the trust record
 * lands, it is false: the chat shows no marker.
 */
export function ranOutsideSandbox(meta: unknown): boolean {
  if (typeof meta !== 'object' || meta === null) return false;
  const localWork = (meta as { readonly local_work?: unknown }).local_work;
  return typeof localWork === 'object' && localWork !== null && (localWork as { readonly ran_outside_sandbox?: unknown }).ran_outside_sandbox === true;
}

/**
 * The answer's turn ran in Full auto (ADR-0061 amendment of 2026-11-26, issue 3079: Main stores the commit's
 * `local_work.full_auto` record on the answer). Its presence is the marker; absent, the chat shows none.
 */
export function ranInFullAuto(meta: unknown): boolean {
  if (typeof meta !== 'object' || meta === null) return false;
  const localWork = (meta as { readonly local_work?: unknown }).local_work;
  if (typeof localWork !== 'object' || localWork === null) return false;
  const record = (localWork as { readonly full_auto?: unknown }).full_auto;
  return typeof record === 'object' && record !== null && !Array.isArray(record);
}

/** What a Local work turn recorded about its folder's project instructions (issue 3130). */
export interface ProjectInstructionsRecord {
  /** The turn's prompt carried them: false when the thread's "Respect project instructions" was off. */
  readonly applied: boolean;
  readonly files: readonly string[];
}

/**
 * The answer's turn recorded whether it applied its folder's project instructions (AGENTS.md; issue 3130: Main
 * stores the commit's `local_work.project_instructions`). Absent (no instruction files, or a server before contract
 * 1.70), the chat shows nothing.
 */
export function projectInstructionsOf(meta: unknown): ProjectInstructionsRecord | undefined {
  if (typeof meta !== 'object' || meta === null) return undefined;
  const localWork = (meta as { readonly local_work?: unknown }).local_work;
  if (typeof localWork !== 'object' || localWork === null) return undefined;
  const record = (localWork as { readonly project_instructions?: unknown }).project_instructions;
  if (typeof record !== 'object' || record === null) return undefined;
  const { applied, files } = record as { readonly applied?: unknown; readonly files?: unknown };
  if (typeof applied !== 'boolean' || !Array.isArray(files)) return undefined;
  return { applied, files: files.filter((file): file is string => typeof file === 'string') };
}

/**
 * The newest answer in the list was produced on a desktop (`meta.executed_by
 * === 'desktop'`). The server refuses to regenerate such an answer
 * (409 `local_work_turn`): the chat does not offer it.
 */
export function lastAnswerRanOnDesktop(rows: readonly unknown[]): boolean {
  for (let index = rows.length - 1; index >= 0; index -= 1) {
    const row = rows[index] as { readonly role?: unknown; readonly meta?: unknown; readonly metadata?: unknown } | null;
    if (row?.role !== 'assistant') continue;
    const meta = (row.meta ?? row.metadata) as { readonly executed_by?: unknown } | undefined;
    return meta?.executed_by === 'desktop';
  }
  return false;
}
