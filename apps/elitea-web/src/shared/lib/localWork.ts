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
