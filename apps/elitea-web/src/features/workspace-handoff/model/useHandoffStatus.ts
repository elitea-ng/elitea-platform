/**
 * Where a thread is in a handoff: `handoff_status`, read when the thread
 * opens and kept current while it is not `LOCAL` (the cloud's turns, a take
 * back requested elsewhere and an erase all move it without this window
 * doing anything). A thread with no server conversation yet has no status.
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';

import type { HandoffIpc, HandoffStatus } from '@/shared/desktop/handoffIpc';

/** How often a thread that is not local is re-read. */
const HANDOFF_POLL_MS = 5000;

/** Prefix of every status read of one thread (invalidate by it). */
export const handoffStatusKey = (conversationUuid: string | undefined) => ['workspace', 'handoff', 'status', conversationUuid ?? ''] as const;

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

/** Is the thread running (or being returned) anywhere but this computer? */
export function isOffLaptop(status: HandoffStatus | undefined): boolean {
  return status !== undefined && status.state !== 'LOCAL';
}
