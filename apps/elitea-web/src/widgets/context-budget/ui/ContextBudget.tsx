/**
 * ui/ContextBudget.tsx — the container: resolves the context status for one
 * conversation and hands the narrowed stats to `ContextBudgetPanel`.
 *
 * Mounted through `ParticipantsWrapper`'s `renderContextBudget` slot (the
 * `features/` layer may not import `widgets/`, so the page supplies the slot —
 * see `pages/chat/index.tsx`). Old-app equivalent:
 * `widgets/context-budget/ui/ContextBudgetInfo.jsx`.
 *
 * Gating, in the same order as the old app:
 *  - no `conversationId` (a brand-new or playback conversation — the wrapper
 *    already nulls the id for those) or no `projectId` -> the query never runs
 *    and nothing renders;
 *  - loading / error / non-object payload -> nothing renders. The old app
 *    returns `null` for all three rather than showing a skeleton or an error;
 *    a rail that briefly shows an empty budget box is worse than one that
 *    appears when it has something to say.
 *
 * SCOPE. The panel is no longer read-only: its header carries the pencil, and
 * the pencil edits the CONTEXT BUDGET — the one number the panel itself
 * reports. The budget resolves through
 * `conversation strategy > the user's defaults > the constants`
 * (`internal/domain/contextsettings`.`Resolve`), and this writes the middle
 * term with `PUT /social/author`, which is the same field and the same request
 * Settings › Memory sends. One setting reachable from two places, rather than
 * two settings that drift.
 *
 * STILL NOT BUILT, and deliberately so: the old app's `ContextStrategyModal`
 * also edits the CONVERSATION's own strategy — its instructions, persona and
 * summary-LLM settings — through `updateContextStrategy`. Note for whoever
 * builds that: the Go route REPLACES the whole `meta.context_strategy` object
 * with the request body (`jsonb_set` in `conversations.go`'s
 * `UpdateContextStrategy`), so a partial form must merge onto the existing
 * strategy or it will silently drop `summary_llm_settings`.
 */
import { useCallback, useState, type ReactNode } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { contextManagementApi } from '@/entities/conversation';
import {
  getGetCurrentAuthorQueryKey,
  updateCurrentAuthor,
  useGetCurrentAuthor,
} from '@/shared/api/generated/social/social';
import { t } from '@/shared/i18n';

import {
  buildContextBudgetUpdate,
  selectMaxContextTokens,
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
        currentMaxTokens={currentBudget(edit.storedMaxTokens, stats.maxTokens)}
        isSaving={edit.isSaving}
        errorMessage={edit.errorMessage}
        onClose={edit.close}
        onSave={edit.save}
      />
    </>
  );
}

/**
 * What the dialog opens on.
 *
 * The reader's OWN default when they have one; otherwise the budget the server
 * resolved for this conversation, so the form starts from the number on screen
 * instead of an empty field. `0` is the server's "context manager off" signal
 * (`lib/contextStatus.ts`, quirk 1), not a budget, so it seeds nothing.
 *
 * A module-level function, not an inline expression: `ContextBudget` sits at
 * the §3.5 cyclomatic-complexity budget (12).
 */
function currentBudget(stored: number | undefined, resolved: number): number | undefined {
  if (stored !== undefined) return stored;
  return resolved === 0 ? undefined : resolved;
}

interface ContextBudgetEdit {
  readonly isOpen: boolean;
  readonly isSaving: boolean;
  readonly errorMessage: string | undefined;
  /** The reader's OWN default, or `undefined` when they have never saved one. */
  readonly storedMaxTokens: number | undefined;
  readonly open: () => void;
  readonly close: () => void;
  readonly save: (maxContextTokens: number) => void;
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
    (maxContextTokens: number) => {
      const write = async () => {
        setIsSaving(true);
        setErrorMessage(undefined);
        try {
          await updateCurrentAuthor(buildContextBudgetUpdate(author, maxContextTokens));
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
    storedMaxTokens: selectMaxContextTokens(author),
    open: useCallback(() => {
      setErrorMessage(undefined);
      setIsOpen(true);
    }, []),
    close: useCallback(() => setIsOpen(false), []),
    save,
  };
}
