/**
 * The queries and writes over one agent's memory (ADR-0050 decision 1b).
 *
 * Every write invalidates every cached read of this agent's memory — the
 * list pages, a single entry and its history — by the list route's key
 * prefix, which the single-entry key shares. A write from the chat chip and
 * one from the Memory tab therefore leave neither view stale.
 */
import { useCallback } from 'react';

import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getListAgentMemoryQueryKey } from '@/shared/api/generated/applications/applications';
import type { AgentMemoryEntry, AgentMemoryList, AgentMemoryUpdate } from '@/shared/api/generated/model';

import {
  changeAgentMemoryEntry,
  clearAllAgentMemory,
  fetchAgentMemory,
  fetchAgentMemoryEntry,
  forgetAgentMemoryEntry,
  setAgentMemoryOptedOut,
} from '../api/agentMemoryApi';
import { filtersToParams, MEMORY_EXPORT_PAGE_SIZE, type MemoryFilters } from '../lib/memoryFilters';

/** The prefix every read of one agent's memory is cached under. */
export function agentMemoryKey(projectId: string, applicationId: number): readonly unknown[] {
  return getListAgentMemoryQueryKey(projectId, applicationId);
}

function enabledFor(projectId: string | undefined, applicationId: number | undefined): boolean {
  return projectId !== undefined && projectId !== '' && applicationId !== undefined && applicationId > 0;
}

/** One page of the filtered list, with what the caller may do. */
export function useAgentMemoryList(projectId: string | undefined, applicationId: number | undefined, filters: MemoryFilters, offset: number) {
  const params = filtersToParams(filters, offset);
  return useQuery<AgentMemoryList>({
    queryKey: [...agentMemoryKey(projectId ?? '', applicationId ?? 0), 'list', params],
    queryFn: () => fetchAgentMemory(projectId ?? '', applicationId ?? 0, params),
    enabled: enabledFor(projectId, applicationId),
    placeholderData: keepPreviousData,
    retry: false,
  });
}

/**
 * What the caller may do with this agent's memory, from a one-row read. The
 * chat chip asks only when it has a pending entry to offer approval for.
 */
export function useAgentMemoryAbilities(projectId: string | undefined, applicationId: number | undefined, enabled: boolean) {
  const query = useQuery<AgentMemoryList>({
    queryKey: [...agentMemoryKey(projectId ?? '', applicationId ?? 0), 'abilities'],
    queryFn: () => fetchAgentMemory(projectId ?? '', applicationId ?? 0, { limit: 1 }),
    enabled: enabled && enabledFor(projectId, applicationId),
    retry: false,
    staleTime: 60_000,
  });
  return { canApprove: query.data?.can_approve === true, canManage: query.data?.can_manage === true };
}

/** One entry as it is now; `history` holds every version, newest first. */
export function useAgentMemoryEntry(projectId: string | undefined, applicationId: number | undefined, entryId: number | undefined) {
  return useQuery<AgentMemoryEntry>({
    queryKey: [...agentMemoryKey(projectId ?? '', applicationId ?? 0), 'entry', entryId],
    queryFn: () => fetchAgentMemoryEntry(projectId ?? '', applicationId ?? 0, entryId ?? 0),
    enabled: enabledFor(projectId, applicationId) && entryId !== undefined && entryId > 0,
    retry: false,
  });
}

/** The writes, each invalidating this agent's cached memory reads. */
export function useAgentMemoryWrites(projectId: string, applicationId: number) {
  const queryClient = useQueryClient();
  const invalidate = useCallback(
    () => queryClient.invalidateQueries({ queryKey: agentMemoryKey(projectId, applicationId) }),
    [queryClient, projectId, applicationId],
  );
  const update = useMutation({
    mutationFn: ({ entryId, change }: { readonly entryId: number; readonly change: AgentMemoryUpdate }) =>
      changeAgentMemoryEntry(projectId, applicationId, entryId, change),
    onSettled: invalidate,
  });
  const remove = useMutation({
    mutationFn: (entryId: number) => forgetAgentMemoryEntry(projectId, applicationId, entryId),
    onSettled: invalidate,
  });
  const clear = useMutation({ mutationFn: () => clearAllAgentMemory(projectId, applicationId), onSettled: invalidate });
  const optOut = useMutation({
    mutationFn: (optedOut: boolean) => setAgentMemoryOptedOut(projectId, applicationId, optedOut),
    onSettled: invalidate,
  });
  return { update, remove, clear, optOut };
}

/**
 * Every entry the filters match, read page by page (the route answers at most
 * 200 a page). Bounded: an agent keeps at most 2,000 active entries, and the
 * loop stops at 50 pages whatever the total says.
 */
export async function readAllAgentMemory(projectId: string, applicationId: number, filters: MemoryFilters): Promise<AgentMemoryEntry[]> {
  const rows: AgentMemoryEntry[] = [];
  for (let page = 0; page < 50; page += 1) {
    const list = await fetchAgentMemory(projectId, applicationId, filtersToParams(filters, rows.length, MEMORY_EXPORT_PAGE_SIZE));
    rows.push(...list.rows);
    if (list.rows.length < MEMORY_EXPORT_PAGE_SIZE || rows.length >= list.total) break;
  }
  return rows;
}
