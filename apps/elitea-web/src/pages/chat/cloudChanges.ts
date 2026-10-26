/**
 * The Files changed view of a cloud thread (ADR-0034 Phase 2): what the
 * cloud turns changed, per turn and since the handoff snapshot, and one
 * file's diff. The worker records the changes when it publishes a turn; the
 * server never lists a path the exclusions or the path deny list match (it
 * counts it in `hidden_files`).
 *
 * The list is read once for the notice's count, and polled only while the
 * view is open. A diff belongs to one commit, so it never goes stale.
 */
import { useQuery } from '@tanstack/react-query';

import { getWorkspaceHandoffChanges, getWorkspaceHandoffDiff } from '@/shared/api/generated/chat/chat';
import type { WorkspaceChangeSet, WorkspaceHandoffChanges } from '@/shared/api/generated/model';

const POLL_MS = 10_000;

const cloudChangesKey = (projectId: string, conversationUuid: string) => ['pages-chat', 'cloud-changes', projectId, conversationUuid] as const;

/** The thread's changes; polled while `open`. */
export function useCloudChanges(projectId: string, conversationUuid: string, open: boolean) {
  return useQuery({
    queryKey: cloudChangesKey(projectId, conversationUuid),
    queryFn: async (): Promise<WorkspaceHandoffChanges> => {
      const response = await getWorkspaceHandoffChanges(projectId, conversationUuid);
      if (response.status !== 200) throw new Error('The changes could not be read.');
      return response.data;
    },
    retry: false,
    refetchInterval: open ? POLL_MS : false,
  });
}

/** `cumulative`: since the handoff snapshot; a number: that turn (0 is the oldest). */
export type CloudScope = 'cumulative' | number;

export interface ScopeView {
  /** The commit a diff of this scope is read at (`head_sha` for the cumulative scope). */
  readonly commit: string | null;
  readonly scope: 'turn' | 'cumulative';
  /** `null`: the scope has no record. */
  readonly set: WorkspaceChangeSet | null;
}

/** What the picked scope shows; an out-of-range turn falls back to the cumulative scope. */
export function scopeView(changes: WorkspaceHandoffChanges, picked: CloudScope): ScopeView {
  const turn = picked === 'cumulative' ? undefined : changes.turns[picked];
  if (turn !== undefined) return { commit: turn.commit, scope: 'turn', set: turn.changes };
  return { commit: changes.head_sha ?? null, scope: 'cumulative', set: changes.cumulative ?? null };
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
