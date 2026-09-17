import { useCallback, useState, type ReactNode } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { contextManagementApi } from '@/entities/conversation';
import {
  getGetCurrentAuthorQueryKey,
  updateCurrentAuthor,
  useGetCurrentAuthor,
} from '@/shared/api/generated/social/social';
import type { ContextBudgetMode } from '@/shared/lib/contextBudget';
import { t } from '@/shared/i18n';

import {
  buildContextBudgetUpdate,
  selectBudgetMode,
  type AuthorContextProfile,
} from '../lib/authorContextUpdate';
import { toContextBudgetStats } from '../lib/contextStatus';
import { ContextBudgetEditDialog } from './ContextBudgetEditDialog';
import { ContextBudgetCollapsed, ContextBudgetPanel } from './ContextBudgetPanel';

/** @public */
export interface ContextBudgetProps {
  /** The conversation to report on. Absent for a new/playback conversation — the widget then renders nothing. */
  readonly conversationId?: string | number | undefined;
  /** The project the conversation belongs to. Supplied by the page, which already resolves it. */
  readonly projectId?: string | number | undefined;
  /**
   * Render the minimal collapsed form (a percentage over a 2.25rem status
   * line) instead of the full card — the rail is 3.25rem wide there and the
   * card does not fit. Baseline: `ContextBudgetInfo.jsx`'s `if (collapsed)
   * return <ContextBudgetCollapsed/>` branch, driven by
   * `Participants.jsx:120`'s `collapsed && !isSmallWindow`.
   */
  readonly collapsed?: boolean;
}

export function ContextBudget({ conversationId, projectId, collapsed = false }: ContextBudgetProps): ReactNode {
  const enabled = conversationId !== undefined && conversationId !== '' && projectId !== undefined && projectId !== '';
  const { data, isPending, isError } = contextManagementApi.useGetStatus(
    { projectId: projectId ?? '', conversationId: conversationId ?? '' },
    { enabled },
  );
  const edit = useContextBudgetEdit();

  if (!enabled || isPending || isError) return null;

  const stats = toContextBudgetStats(data);
  if (!stats) return null;

  if (collapsed) return <ContextBudgetCollapsed stats={stats} />;
  return (
    <>
      <ContextBudgetPanel
        stats={stats}
        onEdit={edit.open}
      />
      <ContextBudgetEditDialog
        open={edit.isOpen}
        currentMode={edit.storedMode}
        isSaving={edit.isSaving}
        errorMessage={edit.errorMessage}
        onClose={edit.close}
        onSave={edit.save}
      />
    </>
  );
}

interface ContextBudgetEdit {
  readonly isOpen: boolean;
  readonly isSaving: boolean;
  readonly errorMessage: string | undefined;
  /** The reader's OWN default, or `undefined` when they have never saved one. */
  readonly storedMode: ContextBudgetMode;
  readonly open: () => void;
  readonly close: () => void;
  readonly save: (budgetMode: ContextBudgetMode) => void;
}

/**
 * The dialog's state and its one write.
 *
 * A hook rather than more lines in the component: `ContextBudget` returns
 * `null` from four early branches, and hooks may not be called after them —
 * this keeps the whole edit path above the first return, as one call.
 *
 * The transport is deliberately the same one `features/settings` uses for the
 * same endpoint — the plain `updateCurrentAuthor` call plus an explicit
 * invalidation of `getGetCurrentAuthorQueryKey()`. Two writers of one blob
 * that disagree about how to write it is how a carry-forward gets missed.
 *
 * The CONTEXT-STATUS query is invalidated too: the panel's number is what the
 * reader just changed, and without this it keeps showing the old budget until
 * something else happens to refetch.
 *
 * That invalidation is made BY THE READER'S OWN KEY FACTORY
 * (`contextManagementApi.statusQueryKey()`), not by a key written out here.
 * It used to be the URL-shaped `['GET', '/elitea_core/context_analytics/
 * prompt_lib']`, a namespace no query in this app is registered under:
 * `invalidateQueries` matches prefixes structurally, matched nothing, and
 * resolved successfully, so the panel went on reporting the budget the reader
 * had just replaced until the next reload — the "two query-key namespaces over
 * one resource" defect, with the write side holding the namespace nobody reads.
 */
function useContextBudgetEdit(): ContextBudgetEdit {
  const queryClient = useQueryClient();
  const { data: authorData } = useGetCurrentAuthor();
  const author = authorData?.data as AuthorContextProfile | undefined;
  const [isOpen, setIsOpen] = useState(false);
  const [isSaving, setIsSaving] = useState(false);
  const [errorMessage, setErrorMessage] = useState<string | undefined>(undefined);

  const save = useCallback(
    (budgetMode: ContextBudgetMode) => {
      const write = async () => {
        setIsSaving(true);
        setErrorMessage(undefined);
        try {
          await updateCurrentAuthor(buildContextBudgetUpdate(author, budgetMode));
          await queryClient.invalidateQueries({ queryKey: getGetCurrentAuthorQueryKey() });
          await queryClient.invalidateQueries({ queryKey: contextManagementApi.statusQueryKey() });
          setIsOpen(false);
        } catch {
          // Handled (§3.6): a refused save is reported inside the dialog, which
          // stays open on the value the reader typed.
          setErrorMessage(t('widgets.contextBudget.edit.failed', 'Failed to save the context settings.'));
        } finally {
          setIsSaving(false);
        }
      };
      void write();
    },
    [author, queryClient],
  );

  return {
    isOpen,
    isSaving,
    errorMessage,
    storedMode: selectBudgetMode(author),
    open: useCallback(() => {
      setErrorMessage(undefined);
      setIsOpen(true);
    }, []),
    close: useCallback(() => setIsOpen(false), []),
    save,
  };
}
