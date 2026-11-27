/** What a Local work turn changed on disk (IPC.md, `turn_changes`): its files, and what undo does not cover. */
export type ChangeStatus = 'added' | 'modified' | 'deleted' | 'renamed';

export interface ChangedFile {
  path: string;
  status: ChangeStatus;
  added: number;
  removed: number;
  /** Unified diff text; may be empty for binary or very large files. */
  diff: string;
  /** No line diff: the file is binary, or too large to keep (its sizes say what changed). */
  binary?: boolean;
  /** Bytes before the turn; absent when it did not exist. */
  size_before?: number;
  /** Bytes now; absent when it was deleted. */
  size_after?: number;
  /** The command that changed it first; absent when a file tool did. */
  command?: string;
}

/** What a turn's checkpoint left out (at most 50 named per list): a change to one of them cannot be undone. */
export interface NotSaved {
  too_large: string[];
  too_large_count: number;
  placeholders: string[];
  placeholder_count: number;
}

export interface TurnChanges {
  files: ChangedFile[];
  /** The newest turn of its folder whose changes stand: "Undo turn" is for it only; an older turn offers "Restore folder to before this turn". */
  latest: boolean;
  /** Its changes were undone (its own undo, or the folder restored to before an earlier turn): nothing to undo. */
  undone: boolean;
  /** What the turn's checkpoint left out; `null` (or absent, an older host) when nothing. */
  not_saved?: NotSaved | null;
  /** A command ran while the folder was too large to scan: its changes may be missing from `files`. */
  commands_unlisted?: boolean;
}
