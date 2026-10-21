/**
 * Local work threads (ADR-0029, ADR-0034): conversations a desktop agent ran over a folder on
 * someone's laptop (`source = 'local_work'`). A desktop can send one to the cloud; its
 * `workspace_executor` then says who runs it:
 *
 * - `local`: the laptop. The thread is a read-only transcript here (the server refuses a turn
 *   it cannot run, 409 `local_work_thread`);
 * - `cloud`: the cloud executor. Any client may post to it through the normal chat route;
 * - `returning`: the laptop is taking it back. Read-only until it lands.
 *
 * This mirrors the web app (apps/elitea-web `shared/lib/localWork.ts`, `pages/chat/handoff*`).
 * Pure functions only: the screens and tests share them.
 */

/** The conversation `source` the desktop writes (the server's `conversations.LocalWorkSource`). */
export const LOCAL_WORK_SOURCE = 'local_work';

export type WorkspaceExecutor = 'local' | 'cloud' | 'returning';

/** The handoff states of `getWorkspaceHandoff` (contract 1.8). */
export const HANDOFF_STATES = ['LOCAL', 'HANDING_OFF', 'CLOUD_IDLE', 'CLOUD_RUNNING', 'RECLAIMING'] as const;

/** First client contract with the handoff routes and `workspace_executor`. */
export const HANDOFF_CONTRACT = { major: 1, minor: 8 } as const;

/** `workspace_executor` of a row or a conversation; absent or unknown means the laptop. */
export function executorOf(value: { workspace_executor?: unknown } | null | undefined): WorkspaceExecutor {
  const executor = value?.workspace_executor;
  return executor === 'cloud' || executor === 'returning' ? executor : 'local';
}

/** The executor a handoff state means (the server's `WorkspaceExecutorOf`): a send still in flight is the laptop's. */
export function executorOfState(state: string | undefined): WorkspaceExecutor | null {
  switch (state) {
    case 'CLOUD_IDLE':
    case 'CLOUD_RUNNING':
      return 'cloud';
    case 'RECLAIMING':
      return 'returning';
    case 'LOCAL':
    case 'HANDING_OFF':
      return 'local';
    default:
      return null;
  }
}

/** True for a conversation the desktop's Local work owns (`source`, else the `meta` it records on create). */
export function isLocalWork(conversation: { source?: unknown; meta?: unknown } | null | undefined): boolean {
  if (conversation?.source === LOCAL_WORK_SOURCE) return true;
  const meta = conversation?.meta;
  return typeof meta === 'object' && meta !== null && typeof (meta as Record<string, unknown>).local_work === 'object' && (meta as Record<string, unknown>).local_work !== null;
}

/** The folder's name the desktop recorded in `meta.local_work.folder_name`, when it did. */
export function folderNameOf(meta: unknown): string | undefined {
  if (typeof meta !== 'object' || meta === null) return undefined;
  const localWork = (meta as Record<string, unknown>).local_work;
  if (typeof localWork !== 'object' || localWork === null) return undefined;
  const name = (localWork as Record<string, unknown>).folder_name;
  return typeof name === 'string' && name.trim() !== '' ? name.trim() : undefined;
}

/**
 * The newest answer of the transcript (oldest first) was produced on a desktop
 * (`meta.executed_by === 'desktop'`). The server refuses to regenerate it
 * (409 `local_work_turn`), so no Regenerate is offered.
 */
export function lastAnswerRanOnDesktop(messages: readonly { role: string; executedBy?: string | null }[]): boolean {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    if (messages[index].role !== 'assistant') continue;
    return messages[index].executedBy === 'desktop';
  }
  return false;
}

/** The list badge of a Local work row: "In cloud", "Returning" or "On laptop". */
export type ExecutorBadge = 'In cloud' | 'Returning' | 'On laptop';

export function executorBadge(executor: WorkspaceExecutor): ExecutorBadge {
  switch (executor) {
    case 'cloud':
      return 'In cloud';
    case 'returning':
      return 'Returning';
    default:
      return 'On laptop';
  }
}

export interface LocalWorkThread {
  executor: WorkspaceExecutor;
  folderName: string | undefined;
  /** The signed-in user created the thread (Take back is theirs alone). */
  isOwner: boolean;
}

/** What the thread screen derives for a Local work conversation (null for an ordinary chat). */
export function describeLocalWork(
  detail: { source?: unknown; meta?: unknown; created_by?: unknown } | null | undefined,
  executor: WorkspaceExecutor,
  selfUserId: number | null,
): LocalWorkThread | null {
  if (!isLocalWork(detail)) return null;
  const owner = detail?.created_by;
  return {
    executor,
    folderName: folderNameOf(detail?.meta),
    isOwner: selfUserId !== null && owner !== undefined && owner !== null && String(owner) === String(selfUserId),
  };
}

/** Take back asks the owner's laptop; the server accepts nobody else. Not while it is already coming back. */
export function canRequestReclaim(thread: LocalWorkThread): boolean {
  return thread.executor === 'cloud' && thread.isOwner;
}

/** Erase cloud copy: the owner, or anyone who may edit the project's settings (the server checks again). */
export function canEraseCloudCopy(thread: LocalWorkThread, canEditProjectSettings: boolean): boolean {
  return (thread.executor === 'cloud' || thread.executor === 'returning') && (thread.isOwner || canEditProjectSettings);
}

/** The permission that lets a non-owner erase a thread's cloud copy. */
export const PROJECT_SETTINGS_EDIT = 'models.project_settings.edit';

/**
 * Why the composer is off for a Local work thread, or null when it works: only the cloud
 * executor takes a message from here; the laptop's and the returning thread are read-only.
 */
export function composerBlockedReason(thread: LocalWorkThread | null): string | null {
  if (thread === null) return null;
  if (thread.executor === 'cloud') return null;
  if (thread.executor === 'returning') return 'Your laptop is taking this thread back.';
  return thread.folderName === undefined
    ? 'This thread runs on your laptop. Send it to the cloud from the desktop app to continue here.'
    : `This thread runs on ${thread.folderName} on your laptop. Send it to the cloud from the desktop app to continue here.`;
}
