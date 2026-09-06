/**
 * All state and handlers for Admin → Budgets. The page component renders; this
 * hook decides.
 *
 * ## Why the edit dialog re-reads the project
 *
 * The listing carries the money but NOT the two policy columns
 * (`budget_period`, `nats_fail_mode`) — they exist only at project scope and
 * the page query does not join them. A dialog seeded from the row would show
 * defaults for both, and saving would then write a value nobody chose over one
 * that was already stored. So opening the dialog starts a per-project read and
 * the dialog stays in its loading state until it lands.
 */
import { useCallback, useMemo, useState } from 'react';

import { t } from '@/shared/i18n';

import {
  adminBudgetFailureReason,
  useClearMemberBudget,
  useClearProjectBudget,
  useMemberBudgets,
  useProjectBudget,
  useProjectBudgets,
  useSaveMemberBudget,
  useSaveProjectBudget,
  type BudgetFormValues,
  type MemberBudgetRow,
  type ProjectBudget,
  type ProjectBudgetRow,
  type ProjectTypeFilter,
} from './api/adminBudgetsApi';
import type { BudgetDialogInitialValues } from './AdminBudgetDialog';

export const ADMIN_BUDGETS_PAGE_SIZE = 20;

/** Which budget the dialog is editing, if any. */
type DialogTarget =
  | { readonly scope: 'project'; readonly projectId: number; readonly name: string }
  | {
      readonly scope: 'member';
      readonly projectId: number;
      readonly userId: number;
      readonly name: string;
      readonly row: MemberBudgetRow;
    };

export interface AdminBudgetsPageState {
  readonly rows: readonly ProjectBudgetRow[];
  readonly total: number;
  readonly counts: Readonly<Record<string, number>>;
  readonly isLoading: boolean;
  readonly loadFailed: boolean;
  readonly page: number;
  readonly search: string;
  readonly projectType: ProjectTypeFilter;
  readonly errorMessage: string;
  readonly savedMessage: string;
  /** Whether any row on this page carries an authored ceiling (gates the G12 banner). */
  readonly anyBudgetAuthored: boolean;

  readonly dialogOpen: boolean;
  readonly dialogScope: 'project' | 'member';
  readonly dialogSubject: string;
  readonly dialogInitial: BudgetDialogInitialValues | undefined;
  readonly dialogLoading: boolean;
  readonly isSaving: boolean;
  readonly saveError: string | undefined;

  readonly membersOpen: boolean;
  readonly membersProjectName: string;
  readonly memberRows: readonly MemberBudgetRow[];
  readonly membersLoading: boolean;
  readonly membersError: string | undefined;

  readonly onSearchChange: (value: string) => void;
  readonly onProjectTypeChange: (value: ProjectTypeFilter) => void;
  readonly onPreviousPage: () => void;
  readonly onNextPage: () => void;
  readonly onEditProject: (row: ProjectBudgetRow) => void;
  readonly onClearProject: (row: ProjectBudgetRow) => void;
  readonly onOpenMembers: (row: ProjectBudgetRow) => void;
  readonly onCloseMembers: () => void;
  readonly onEditMember: (row: MemberBudgetRow) => void;
  readonly onClearMember: (row: MemberBudgetRow) => void;
  readonly onDialogClose: () => void;
  readonly onDialogSubmit: (values: BudgetFormValues) => void;
  readonly onDismissError: () => void;
  readonly onDismissSaved: () => void;
}

/** Has anybody authored a ceiling on this page? Gates the enforcement banner. */
function hasAuthoredCeiling(rows: readonly ProjectBudgetRow[]): boolean {
  return rows.some((row) => row.monthly_limit !== null && row.monthly_limit !== undefined);
}

/**
 * The dialog's seed values.
 *
 * A PROJECT edit returns `undefined` until the per-project read lands, which is
 * what keeps the dialog in its loading state: seeding the policy fields from
 * defaults and then saving would overwrite a stored policy the form never
 * showed anyone. A MEMBER edit has no policy fields, so the listing row it was
 * launched from is already the whole truth.
 */
function budgetDialogSeed(
  target: DialogTarget | undefined,
  stored: ProjectBudget | undefined,
): BudgetDialogInitialValues | undefined {
  if (target === undefined) return undefined;
  if (target.scope === 'member') {
    return {
      monthlyLimit: target.row.monthly_limit ?? null,
      enabled: target.row.enabled ?? true,
      softAlertPct: target.row.warning_pct ?? null,
    };
  }
  if (stored === undefined) return undefined;
  return {
    monthlyLimit: stored.monthly_limit ?? null,
    enabled: stored.enabled ?? true,
    softAlertPct: stored.warning_pct ?? null,
    budgetPeriod: stored.budget_period,
    natsFailMode: stored.nats_fail_mode,
  };
}

/** The listing half of the page state, derived from one query result. */
interface ListingView {
  readonly rows: readonly ProjectBudgetRow[];
  readonly total: number;
  readonly counts: Readonly<Record<string, number>>;
  readonly isLoading: boolean;
  readonly loadFailed: boolean;
  readonly anyBudgetAuthored: boolean;
}

function listingView(query: ReturnType<typeof useProjectBudgets>): ListingView {
  const data = query.data;
  const rows = data === undefined ? [] : data.rows;
  return {
    rows,
    total: data === undefined ? 0 : data.total,
    counts: data === undefined ? {} : data.counts,
    isLoading: query.isLoading,
    loadFailed: query.isError,
    anyBudgetAuthored: hasAuthoredCeiling(rows),
  };
}

/** The member-drawer half, derived from the drawer's project and its query. */
interface MembersView {
  readonly membersOpen: boolean;
  readonly membersProjectName: string;
  readonly memberRows: readonly MemberBudgetRow[];
  readonly membersLoading: boolean;
  readonly membersError: string | undefined;
}

function membersView(
  project: ProjectBudgetRow | undefined,
  query: ReturnType<typeof useMemberBudgets>,
): MembersView {
  const open = project !== undefined;
  return {
    membersOpen: open,
    membersProjectName: project === undefined ? '' : project.display_name,
    memberRows: query.data === undefined ? [] : query.data.rows,
    membersLoading: open && query.isLoading,
    membersError: query.isError
      ? t('pages.admin.budgets.error.members', 'Failed to load member budgets.')
      : undefined,
  };
}

/** The four writes, plus the two report sinks their outcomes land in. */
interface BudgetWriteSinks {
  readonly onSaved: (kind: string) => void;
  readonly onListError: (message: string) => void;
  readonly onSaveError: (message: string) => void;
  readonly onDone: () => void;
}

function useBudgetWrites(sinks: BudgetWriteSinks) {
  const saveProject = useSaveProjectBudget();
  const clearProject = useClearProjectBudget();
  const saveMember = useSaveMemberBudget();
  const clearMember = useClearMemberBudget();

  const clearFailed = useCallback(
    (error: unknown) => {
      sinks.onListError(
        adminBudgetFailureReason(error) ??
          t('pages.admin.budgets.error.clear', 'Failed to clear the budget.'),
      );
    },
    [sinks],
  );

  const saveHandlers = useMemo(
    () => ({
      onSuccess: () => {
        sinks.onDone();
        sinks.onSaved('saved');
      },
      onError: (error: unknown) =>
        sinks.onSaveError(
          adminBudgetFailureReason(error) ??
            t('pages.admin.budgets.error.save', 'Failed to save the budget.'),
        ),
    }),
    [sinks],
  );

  const clearHandlers = useMemo(
    () => ({ onSuccess: () => sinks.onSaved('cleared'), onError: clearFailed }),
    [clearFailed, sinks],
  );

  return {
    isSaving: saveProject.isPending || saveMember.isPending,
    saveProject: useCallback(
      (projectId: number, values: BudgetFormValues) =>
        saveProject.mutate({ projectId, values }, saveHandlers),
      [saveHandlers, saveProject],
    ),
    saveMember: useCallback(
      (projectId: number, userId: number, values: BudgetFormValues) =>
        saveMember.mutate({ projectId, userId, values }, saveHandlers),
      [saveHandlers, saveMember],
    ),
    clearProject: useCallback(
      (projectId: number) => clearProject.mutate(projectId, clearHandlers),
      [clearHandlers, clearProject],
    ),
    clearMember: useCallback(
      (projectId: number, userId: number) =>
        clearMember.mutate({ projectId, userId }, clearHandlers),
      [clearHandlers, clearMember],
    ),
  };
}

export function useAdminBudgetsPage(): AdminBudgetsPageState {
  const [page, setPage] = useState(0);
  const [search, setSearch] = useState('');
  const [projectType, setProjectType] = useState<ProjectTypeFilter>('all');
  const [target, setTarget] = useState<DialogTarget | undefined>(undefined);
  const [membersProject, setMembersProject] = useState<ProjectBudgetRow | undefined>(undefined);
  const [errorMessage, setErrorMessage] = useState('');
  const [savedMessage, setSavedMessage] = useState('');
  const [saveError, setSaveError] = useState<string | undefined>(undefined);

  const listing = useProjectBudgets({
    limit: ADMIN_BUDGETS_PAGE_SIZE,
    offset: page * ADMIN_BUDGETS_PAGE_SIZE,
    search,
    projectType,
  });

  // The per-project read that backs the dialog's policy fields. It runs for a
  // project edit only; a member edit has no policy fields to seed.
  const editingProjectId = target?.scope === 'project' ? target.projectId : undefined;
  const projectBudget = useProjectBudget(editingProjectId);

  const membersProjectId = membersProject?.project_id;
  const members = useMemberBudgets(membersProjectId);

  const closeDialog = useCallback(() => {
    setTarget(undefined);
    setSaveError(undefined);
  }, []);

  const sinks = useMemo(
    (): BudgetWriteSinks => ({
      onSaved: setSavedMessage,
      onListError: setErrorMessage,
      onSaveError: setSaveError,
      onDone: () => setTarget(undefined),
    }),
    [],
  );
  const writes = useBudgetWrites(sinks);

  const onEditMember = useCallback(
    (row: MemberBudgetRow) => {
      if (membersProjectId === undefined) return;
      setSaveError(undefined);
      setTarget({
        scope: 'member',
        projectId: membersProjectId,
        userId: row.user_id,
        name: row.email ?? row.name ?? String(row.user_id),
        row,
      });
    },
    [membersProjectId],
  );

  const onClearMember = useCallback(
    (row: MemberBudgetRow) => {
      if (membersProjectId === undefined) return;
      writes.clearMember(membersProjectId, row.user_id);
    },
    [membersProjectId, writes],
  );

  const onDialogSubmit = useCallback(
    (values: BudgetFormValues) => {
      if (target === undefined) return;
      if (target.scope === 'project') {
        writes.saveProject(target.projectId, values);
        return;
      }
      writes.saveMember(target.projectId, target.userId, values);
    },
    [target, writes],
  );

  // A filter change invalidates the offset: page 3 of the old result set is not
  // page 3 of the new one, and an out-of-range offset renders empty.
  const onSearchChange = useCallback((value: string) => {
    setSearch(value);
    setPage(0);
  }, []);
  const onProjectTypeChange = useCallback((value: ProjectTypeFilter) => {
    setProjectType(value);
    setPage(0);
  }, []);

  return {
    ...listingView(listing),
    ...membersView(membersProject, members),
    page,
    search,
    projectType,
    errorMessage,
    savedMessage,

    dialogOpen: target !== undefined,
    dialogScope: target === undefined ? 'project' : target.scope,
    dialogSubject: target === undefined ? '' : target.name,
    dialogInitial: budgetDialogSeed(target, projectBudget.data),
    dialogLoading: editingProjectId !== undefined && projectBudget.isLoading,
    isSaving: writes.isSaving,
    saveError,

    onSearchChange,
    onProjectTypeChange,
    onPreviousPage: useCallback(() => setPage((current) => Math.max(0, current - 1)), []),
    onNextPage: useCallback(() => setPage((current) => current + 1), []),
    onEditProject: useCallback((row: ProjectBudgetRow) => {
      setSaveError(undefined);
      setTarget({ scope: 'project', projectId: row.project_id, name: row.display_name });
    }, []),
    onClearProject: useCallback(
      (row: ProjectBudgetRow) => writes.clearProject(row.project_id),
      [writes],
    ),
    onOpenMembers: useCallback((row: ProjectBudgetRow) => setMembersProject(row), []),
    onCloseMembers: useCallback(() => setMembersProject(undefined), []),
    onEditMember,
    onClearMember,
    onDialogClose: closeDialog,
    onDialogSubmit,
    onDismissError: useCallback(() => setErrorMessage(''), []),
    onDismissSaved: useCallback(() => setSavedMessage(''), []),
  };
}
