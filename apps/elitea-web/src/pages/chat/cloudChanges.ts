/**
 * The Files changed view of a cloud thread (ADR-0034 Phase 2): what the
 * cloud turns changed, per turn and since the handoff snapshot, and one
 * file's diff. The worker records the changes when it publishes a turn; the
 * server never lists a path the exclusions or the path deny list match,
 * unless the send explicitly included it (never a path deny one); it counts
 * the hidden ones in `hidden_files`.
 *
 * The list is read for the notice's count and re-read on every handoff frame
 * of the thread (a cloud turn starting or ending); it is polled every 10 s
 * while the view is open, and every 30 s while a cloud turn runs (the
 * fallback when the project stream is not delivered). A diff belongs to one
 * commit, so it never goes stale.
 */
import { useQuery } from '@tanstack/react-query';

import { handoffChangesKey } from '@/shared/api/workspaceHandoffApi';

import { getWorkspaceHandoffChanges, getWorkspaceHandoffDiff } from '@/shared/api/generated/chat/chat';
import type { WorkspaceChangeSet, WorkspaceHandoffChanges } from '@/shared/api/generated/model';

const OPEN_POLL_MS = 10_000;
const RUNNING_POLL_MS = 30_000;

/** The thread's changes; polled while `open`, and gently while a cloud turn is `running`. */
export function useCloudChanges(projectId: string, conversationUuid: string, open: boolean, running = false) {
  return useQuery({
    queryKey: handoffChangesKey(projectId, conversationUuid),
    queryFn: async (): Promise<WorkspaceHandoffChanges> => {
      const response = await getWorkspaceHandoffChanges(projectId, conversationUuid);
      if (response.status !== 200) throw new Error('The changes could not be read.');
      return response.data;
    },
    retry: false,
    refetchInterval: open ? OPEN_POLL_MS : running ? RUNNING_POLL_MS : false,
  });
}

/** `cumulative`: since the handoff snapshot; otherwise the commit of one turn. */
export type CloudScope = 'cumulative' | { readonly commit: string };

export interface ScopeView {
  /** The commit a diff of this scope is read at (`head_sha` for the cumulative scope). */
  readonly commit: string | null;
  readonly scope: 'turn' | 'cumulative';
  /** `null`: the scope has no record. */
  readonly set: WorkspaceChangeSet | null;
}

/** What the picked scope shows; a turn no longer listed falls back to the cumulative scope. */
export function scopeView(changes: WorkspaceHandoffChanges, picked: CloudScope): ScopeView {
  const turn = picked === 'cumulative' ? undefined : changes.turns.find((candidate) => candidate.commit === picked.commit);
  if (turn !== undefined) return { commit: turn.commit, scope: 'turn', set: turn.changes };
  return { commit: changes.cumulative_commit ?? changes.head_sha ?? null, scope: 'cumulative', set: changes.cumulative ?? null };
}

/** A turn's label number: its number on the chain from the server, else its listed position. */
export function turnNumber(changes: WorkspaceHandoffChanges, index: number): number {
  const number = changes.turns[index]?.number;
  return typeof number === 'number' ? number : index + 1;
}

/** How many files the notice's button counts: what changed since the handoff, listed or not. */
export function changedCount(changes: WorkspaceHandoffChanges | undefined): number {
  const set = changes?.cumulative;
  if (set === undefined || set === null) return 0;
  return set.files.length + set.omitted_files;
}

export interface DiffTarget {
  readonly commit: string;
  readonly scope: 'turn' | 'cumulative';
  readonly path: string;
}

/** One file's diff, read when `target` is set. */
export function useCloudDiff(projectId: string, conversationUuid: string, target: DiffTarget | null) {
  return useQuery({
    queryKey: ['pages-chat', 'cloud-diff', projectId, conversationUuid, target?.commit, target?.scope, target?.path] as const,
    queryFn: async (): Promise<string> => {
      if (target === null) return '';
      const response = await getWorkspaceHandoffDiff(projectId, conversationUuid, target);
      if (response.status !== 200) throw new Error('The diff could not be read.');
      return response.data.diff;
    },
    enabled: target !== null,
    retry: false,
    staleTime: Infinity,
  });
}
