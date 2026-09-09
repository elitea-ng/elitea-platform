/**
 * Split out of `ui/memory/LongTermMemoryManagement.tsx` to stay under the
 * component-complexity budget (§3.5) — same "hooks own the mutations, the
 * component owns only the JSX" split `widgets/chat-box/ui/hooks/
 * useChatBoxActions.ts` establishes for the same reason.
 *
 * Owns: the list query, the four CRUD mutations, the master-toggle bulk
 * action, and every dialog/toast open-state — everything
 * `LongTermMemoryManagement.tsx` needs is one call to
 * `useLongTermMemoryManagement(projectId)` away.
 */
import { useCallback, useMemo, useState } from 'react';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { t } from '@/shared/i18n';
import type { MemoryEntry, MemoryEntryList, MemoryEntryWriteRequest } from '@/shared/api/generated/model';
import {
  clearMemories,
  createMemory,
  deleteMemory,
  getListMemoriesQueryKey,
  updateMemory,
  useListMemories,
} from '@/shared/api/generated/chat/chat';

import type { LongTermMemoryFormValues } from '../../ui/memory/LongTermMemoryFormDialog';
import type { LongTermMemoryViewRow } from '../../ui/memory/LongTermMemoryTable';
import { useLongTermMemoryPermissions } from './useLongTermMemoryPermissions';
import { formatMemoryTags, memoryServerErrorMessage, parseMemoryTags } from './longTermMemoryHelpers';

const EMPTY_MEMORIES: MemoryEntry[] = [];

export interface LongTermMemoryToastState {
  readonly severity: 'success' | 'error';
  readonly message: string;
}

function toViewRow(entry: MemoryEntry): LongTermMemoryViewRow {
  return { id: entry.id, content: entry.content, tags: entry.tags, enabled: entry.enabled };
}

function writeBody(content: string, tags: string, sourceConversationId: string | undefined, enabled: boolean): MemoryEntryWriteRequest {
  return {
    content,
    tags: parseMemoryTags(tags),
    ...(sourceConversationId ? { source_conversation_id: sourceConversationId } : {}),
    enabled,
  };
}

export function useLongTermMemoryManagement(projectId: string) {
  const { canList, canWrite } = useLongTermMemoryPermissions(projectId);
  const queryClient = useQueryClient();

  const [search, setSearch] = useState('');
  const listQuery = useListMemories(projectId, { q: search || undefined }, { query: { enabled: !!projectId && canList } });
  const listBody = listQuery.data?.data as MemoryEntryList | undefined;
  const entries = listBody?.items ?? EMPTY_MEMORIES;
  const rows = useMemo(() => entries.map(toViewRow), [entries]);
  const anyEnabled = entries.some((entry) => entry.enabled);

  const invalidateList = useCallback(
    () => void queryClient.invalidateQueries({ queryKey: getListMemoriesQueryKey(projectId) }),
    [queryClient, projectId],
  );

  const createMutation = useMutation({
    mutationFn: (body: MemoryEntryWriteRequest) => createMemory(projectId, body),
    onSuccess: invalidateList,
  });
  const updateMutation = useMutation({
    mutationFn: ({ memoryId, body }: { memoryId: string; body: MemoryEntryWriteRequest }) => updateMemory(projectId, memoryId, body),
    onSuccess: invalidateList,
  });
  const deleteMutation = useMutation({
    mutationFn: (memoryId: string) => deleteMemory(projectId, memoryId),
    onSuccess: invalidateList,
  });
  const clearAllMutation = useMutation({
    mutationFn: () => clearMemories(projectId),
    onSuccess: invalidateList,
  });

  const [toast, setToast] = useState<LongTermMemoryToastState | null>(null);
  const closeToast = useCallback(() => setToast(null), []);
  const onError = useCallback((message: string) => () => setToast({ severity: 'error', message }), []);

  const [formOpen, setFormOpen] = useState(false);
  const [editingEntry, setEditingEntry] = useState<MemoryEntry | null>(null);
  const [formServerError, setFormServerError] = useState<string | undefined>(undefined);

  const openCreate = useCallback(() => {
    setEditingEntry(null);
    setFormServerError(undefined);
    setFormOpen(true);
  }, []);
  const openEdit = useCallback(
    (row: LongTermMemoryViewRow) => {
      setEditingEntry(entries.find((entry) => entry.id === row.id) ?? null);
      setFormServerError(undefined);
      setFormOpen(true);
    },
    [entries],
  );
  const closeForm = useCallback(() => setFormOpen(false), []);

  const submitForm = useCallback(
    (values: LongTermMemoryFormValues) => {
      setFormServerError(undefined);
      const body = writeBody(values.content, values.tags, editingEntry?.source_conversation_id ?? undefined, values.enabled);
      const handleError = (fallback: string) => (error: unknown) => {
        const message = memoryServerErrorMessage(error, fallback);
        setFormServerError(message);
        onError(message)();
      };
      if (editingEntry) {
        updateMutation.mutate(
          { memoryId: editingEntry.id, body },
          {
            onSuccess: () => setFormOpen(false),
            onError: handleError(t('settings.longTermMemory.error.updateFailed', 'Failed to update the memory')),
          },
        );
        return;
      }
      createMutation.mutate(body, {
        onSuccess: () => setFormOpen(false),
        onError: handleError(t('settings.longTermMemory.error.createFailed', 'Failed to save the memory')),
      });
    },
    [editingEntry, createMutation, updateMutation, onError],
  );

  const toggleEnabled = useCallback(
    (row: LongTermMemoryViewRow, nextEnabled: boolean) => {
      const found = entries.find((entry) => entry.id === row.id);
      if (!found) return;
      updateMutation.mutate(
        { memoryId: row.id, body: writeBody(found.content, formatMemoryTags(found.tags), found.source_conversation_id ?? undefined, nextEnabled) },
        { onError: onError(t('settings.longTermMemory.error.updateFailed', 'Failed to update the memory')) },
      );
    },
    [entries, updateMutation, onError],
  );

  const deleteRow = useCallback(
    (row: LongTermMemoryViewRow) => {
      deleteMutation.mutate(row.id, { onError: onError(t('settings.longTermMemory.error.deleteFailed', 'Failed to delete the memory')) });
    },
    [deleteMutation, onError],
  );

  const [confirmClearAll, setConfirmClearAll] = useState(false);
  const runClearAll = useCallback(() => {
    setConfirmClearAll(false);
    clearAllMutation.mutate(undefined, {
      onSuccess: () => setToast({ severity: 'success', message: t('settings.longTermMemory.clearAllSuccess', 'All memories cleared') }),
      onError: onError(t('settings.longTermMemory.error.clearAllFailed', 'Failed to clear memories')),
    });
  }, [clearAllMutation, onError]);

  const [isBulkToggling, setIsBulkToggling] = useState(false);
  const toggleAll = useCallback(
    async (nextEnabled: boolean) => {
      const mismatched = entries.filter((entry) => entry.enabled !== nextEnabled);
      if (mismatched.length === 0) return;
      setIsBulkToggling(true);
      try {
        await Promise.all(
          mismatched.map((entry) =>
            updateMemory(projectId, entry.id, writeBody(entry.content, formatMemoryTags(entry.tags), entry.source_conversation_id ?? undefined, nextEnabled)),
          ),
        );
        invalidateList();
      } catch (error) {
        onError(memoryServerErrorMessage(error, t('settings.longTermMemory.error.toggleAllFailed', 'Failed to update all memories')))();
      } finally {
        setIsBulkToggling(false);
      }
    },
    [entries, projectId, invalidateList, onError],
  );

  const isMutating = createMutation.isPending || updateMutation.isPending || deleteMutation.isPending || isBulkToggling;

  return {
    canWrite,
    search,
    setSearch,
    rows,
    anyEnabled,
    isLoading: listQuery.isFetching,
    isMutating,
    isBulkToggling,
    toggleAll,
    toast,
    closeToast,
    formOpen,
    editingEntry,
    formServerError,
    openCreate,
    openEdit,
    closeForm,
    submitForm,
    isSavingForm: createMutation.isPending || updateMutation.isPending,
    toggleEnabled,
    deleteRow,
    confirmClearAll,
    setConfirmClearAll,
    runClearAll,
  };
}
