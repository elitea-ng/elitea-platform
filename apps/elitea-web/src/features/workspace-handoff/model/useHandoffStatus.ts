/**
 * Where a thread is in a handoff: `handoff_status`, read when the thread
 * opens and kept current while it is not `LOCAL` (the cloud's turns, a take
 * back requested elsewhere and an erase all move it without this window
 * doing anything). A thread with no server conversation yet has no status.
 *
 * The sidebar badges many rows at once: `useHandoffStatusMany` reads them in
 * one `handoff_status_many` call (the host answers from its own records and
 * caches the server's answer 15 s), each row taking its status from it.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';

import type { HandoffIpc, HandoffStatus } from '@/shared/desktop/handoffIpc';

/** How often a thread that is not local is re-read. */
const HANDOFF_POLL_MS = 5000;

/** Prefix of every status read of one thread (invalidate by it). */
export const handoffStatusKey = (conversationUuid: string | undefined) => ['workspace', 'handoff', 'status', (conversationUuid ?? '').toLowerCase()] as const;

export interface UseHandoffStatus {
  /** `undefined` while the first read is pending, or when it failed. */
  status: HandoffStatus | undefined;
  /** Read it again now (after a send, a take back or an erase). */
  refresh: () => Promise<unknown>;
}

export function useHandoffStatus(ipc: HandoffIpc | undefined, conversationUuid: string | undefined, projectId?: number): UseHandoffStatus {
  const queryClient = useQueryClient();
  const enabled = ipc !== undefined && conversationUuid !== undefined && conversationUuid !== '';
  const query = useQuery({
    // The project is part of the key: with it the host asks the server about a thread it holds no record of; without it, it answers from its own records only.
    queryKey: [...handoffStatusKey(conversationUuid), projectId ?? 'local'],
    queryFn: () => (ipc as HandoffIpc).status(conversationUuid as string, projectId),
    enabled,
    retry: false,
    refetchOnWindowFocus: true,
    refetchInterval: (current) => (current.state.data !== undefined && current.state.data.state !== 'LOCAL' ? HANDOFF_POLL_MS : false),
  });
  return {
    status: enabled ? query.data : undefined,
    refresh: () => queryClient.invalidateQueries({ queryKey: handoffStatusKey(conversationUuid) }),
  };
}

/** Prefix of every batched status read (the sidebar's): invalidate it when a thread moves. */
export const handoffStatusManyKey = () => ['workspace', 'handoff', 'status-many'] as const;

/** How long a batch is fresh, and how often the sidebar reads it again. */
const STATUS_MANY_STALE_MS = 15_000;
const STATUS_MANY_POLL_MS = 20_000;
const EMPTY_STATUSES: Readonly<Record<string, HandoffStatus>> = Object.freeze({});

/** The statuses of many threads in one host call, by UUID; empty while pending or when it failed. */
export function useHandoffStatusMany(ipc: HandoffIpc | undefined, conversationUuids: readonly string[]): Readonly<Record<string, HandoffStatus>> {
  const uuids = [...new Set(conversationUuids)].sort();
  const enabled = ipc !== undefined && uuids.length > 0;
  const query = useQuery({
    queryKey: [...handoffStatusManyKey(), uuids],
    queryFn: () => (ipc as HandoffIpc).statusMany(uuids),
    enabled,
    retry: false,
    staleTime: STATUS_MANY_STALE_MS,
    refetchInterval: STATUS_MANY_POLL_MS,
    refetchOnWindowFocus: true,
    // A row added or removed changes the key: keep showing the badges meanwhile.
    placeholderData: (previous) => previous,
  });
  return (enabled ? query.data : undefined) ?? EMPTY_STATUSES;
}

/** Is the thread running (or being returned) anywhere but this computer? */
export function isOffLaptop(status: HandoffStatus | undefined): boolean {
  return status !== undefined && status.state !== 'LOCAL';
}
