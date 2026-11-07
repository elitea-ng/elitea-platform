/**
 * The Memory tab's state: the filters and the page, the list read, the
 * dialogs that are open, and what each row action does. Split out of
 * `ui/MemoryPanel.tsx` so the component holds only the layout (§3.5).
 */
import { useCallback, useMemo, useRef, useState } from 'react';

import type { AgentMemoryEntry, SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { t } from '@/shared/i18n';
import { triggerBlobDownload } from '@/shared/lib/download';

import { EMPTY_MEMORY_FILTERS, MEMORY_PAGE_SIZE, memoryErrorMessage, type MemoryFilters } from '../lib/memoryFilters';
import type { MemoryRowAction } from '../ui/MemoryEntryRow';
import { readAllAgentMemory, useAgentMemoryList, useAgentMemoryWrites } from './useAgentMemory';

const NO_ROWS: readonly AgentMemoryEntry[] = [];

/** The signed-in user's id, as `useGetCurrentAuthor` answers it (the same read the participants list uses). */
function useCurrentUserId(): number | undefined {
  const author = useGetCurrentAuthor();
  const id = Number((author.data?.data as SocialAuthorProfile | undefined)?.id);
  return Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

export function useMemoryPanel(projectId: string, applicationId: number) {
  const userId = useCurrentUserId();
  const [filters, setFiltersState] = useState<MemoryFilters>(EMPTY_MEMORY_FILTERS);
  const [offset, setOffset] = useState(0);
  const list = useAgentMemoryList(projectId, applicationId, filters, offset);
  const writes = useAgentMemoryWrites(projectId, applicationId);
  const [editing, setEditing] = useState<AgentMemoryEntry | undefined>(undefined);
  const [historyOf, setHistoryOf] = useState<number | undefined>(undefined);
  const [deleting, setDeleting] = useState<AgentMemoryEntry | undefined>(undefined);
  const [confirmClear, setConfirmClear] = useState(false);
  const [notice, setNotice] = useState<{ readonly severity: 'success' | 'error'; readonly message: string } | undefined>(undefined);
  const [exporting, setExporting] = useState(false);

  // Every name the list has answered stays known, so the person filter keeps
  // offering someone after a filter or a page leaves them off the rows.
  const known = useRef(new Map<number, string>());
  const people = list.data?.people;
  const names = useMemo(() => {
    for (const person of people ?? []) known.current.set(person.id, person.name);
    return new Map(known.current);
  }, [people]);

  const setFilters = useCallback((next: MemoryFilters) => {
    setFiltersState(next);
    setOffset(0);
  }, []);

  const fail = useCallback(
    (fallback: string) => (error: unknown) => setNotice({ severity: 'error', message: memoryErrorMessage(error, fallback) }),
    [],
  );

  const onAction = useCallback(
    (action: MemoryRowAction, entry: AgentMemoryEntry) => {
      const change = { approve: { status: 'active' }, reject: { status: 'archived' }, archive: { status: 'archived' }, restore: { status: 'active' }, pin: { pinned: true }, unpin: { pinned: false } } as const;
      if (action === 'edit') setEditing(entry);
      else if (action === 'history') setHistoryOf(entry.id);
      else if (action === 'delete') setDeleting(entry);
      else writes.update.mutate({ entryId: entry.id, change: change[action] }, { onError: fail(t('features.agentMemory.error.update', 'The memory was not changed. Try again.')) });
    },
    [writes.update, fail],
  );

  const confirmDelete = useCallback(() => {
    if (deleting === undefined) return;
    writes.remove.mutate(deleting.id, {
      onSuccess: () => setDeleting(undefined),
      onError: (error) => {
        setDeleting(undefined);
        fail(t('features.agentMemory.error.delete', 'The memory was not deleted. Try again.'))(error);
      },
    });
  }, [deleting, writes.remove, fail]);

  const runClear = useCallback(() => {
    setConfirmClear(false);
    writes.clear.mutate(undefined, {
      onSuccess: (cleared) => setNotice({ severity: 'success', message: t('features.agentMemory.cleared', '{{count}} memories deleted.', { count: cleared }) }),
      onError: fail(t('features.agentMemory.error.clear', 'Nothing was deleted. Try again.')),
    });
  }, [writes.clear, fail]);

  const toggleOptOut = useCallback(
    (optedOut: boolean) => writes.optOut.mutate(optedOut, { onError: fail(t('features.agentMemory.error.optOut', 'The setting was not saved. Try again.')) }),
    [writes.optOut, fail],
  );

  const exportAll = useCallback(async () => {
    setExporting(true);
    try {
      const rows = await readAllAgentMemory(projectId, applicationId, filters);
      const body = JSON.stringify({ application_id: applicationId, exported_at: new Date().toISOString(), entries: rows }, null, 2);
      triggerBlobDownload(new Blob([body], { type: 'application/json' }), `agent-${applicationId}-memory.json`);
    } catch (error) {
      fail(t('features.agentMemory.error.export', 'The export failed. Try again.'))(error);
    } finally {
      setExporting(false);
    }
  }, [projectId, applicationId, filters, fail]);

  const total = list.data?.total ?? 0;
  return {
    userId,
    filters,
    setFilters,
    list,
    rows: list.data?.rows ?? NO_ROWS,
    total,
    names,
    abilities: {
      userId,
      canManage: list.data?.can_manage === true,
      canDelete: list.data?.can_delete === true,
      canApprove: list.data?.can_approve === true,
    },
    optedOut: list.data?.opted_out === true,
    busy: writes.update.isPending || writes.remove.isPending,
    page: { offset, hasPrevious: offset > 0, hasNext: offset + MEMORY_PAGE_SIZE < total, previous: () => setOffset(Math.max(0, offset - MEMORY_PAGE_SIZE)), next: () => setOffset(offset + MEMORY_PAGE_SIZE) },
    dialogs: { editing, closeEdit: () => setEditing(undefined), historyOf, closeHistory: () => setHistoryOf(undefined), deleting, closeDelete: () => setDeleting(undefined), confirmDelete, confirmClear, openClear: () => setConfirmClear(true), closeClear: () => setConfirmClear(false), runClear },
    onAction,
    toggleOptOut,
    optOutPending: writes.optOut.isPending,
    exportAll,
    exporting,
    clearing: writes.clear.isPending,
    notice,
    closeNotice: () => setNotice(undefined),
  };
}
