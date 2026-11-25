/**
 * Read-only mode as one primitive (ADR-0059 decision 6, "Read and write are
 * separate on screen"; R3.4a, issue 2534).
 *
 * One answer per console item, from the capability document (R3.1b), never
 * from a permission name: `write`, `read` or `none`. Three pieces use it:
 *
 *  - `useAdminItemAccess(itemId)`: the answer itself.
 *  - `useAdminWritable(itemId)`: the hook form for a page's own state hook.
 *    Its `guard` wraps every mutation the page runs, so at `read` a mutation
 *    sends nothing, whatever control reached it. The mutation guard
 *    (`__tests__/mutationWrapper.guard.test.ts`) fails on an admin page that
 *    calls a mutation hook outside `guard(...)`.
 *  - `AdminWritable` (`./AdminWritable.tsx`): the wrapper a page renders. It
 *    hands the answer to every control under it (`useAdminWriteControl`),
 *    and its "View only" notice names the roles that can change the page.
 *
 * At `read` a control is DISABLED, not hidden: the person sees what a role
 * with `write` can do there. `none` means the page is not reachable at all
 * (the console shows the access-denied page instead), so it disables too.
 *
 * FAILURE MUST NOT READ AS NO ACCESS. While the document is unknown (pending,
 * failed) the answer is `undefined` and every control stays enabled: the
 * server answers each write for itself, exactly as it did before the console
 * knew levels. A failed read never turns a writer's page read-only.
 */
import { createContext, useContext } from 'react';

import { t } from '@/shared/i18n';

import { useAdminConsole } from './api/adminConsoleApi';

export type AdminItemAccess = 'read' | 'write' | 'none';

/** The item's level from the capability document; `undefined` while the document is unknown. */
export function useAdminItemAccess(itemId: string): AdminItemAccess | undefined {
  const level = useAdminConsole().itemLevel(itemId);
  if (level === undefined) return undefined;
  return level ?? 'none';
}

/** Whether an answer lets the caller change the item. Unknown does: the server decides. */
function writableAt(access: AdminItemAccess | undefined): boolean {
  return access !== 'read' && access !== 'none';
}

/** The shape `guard` accepts: a TanStack mutation result, or anything with its two calls. */
interface GuardableMutation {
  readonly mutate: (...args: never[]) => unknown;
  readonly mutateAsync: (...args: never[]) => Promise<unknown>;
}

/** The refusal `mutateAsync` rejects with when the item is not writable. */
class AdminViewOnlyError extends Error {
  readonly itemId: string;

  constructor(itemId: string) {
    super(`admin console: ${itemId} is view only for this caller`);
    this.name = 'AdminViewOnlyError';
    this.itemId = itemId;
  }
}

function guardMutation<M extends GuardableMutation>(mutation: M, itemId: string, writable: boolean): M {
  if (writable) return mutation;
  // The controls are disabled, so this is the belt behind them: a control
  // that forgot `useAdminWriteControl` still sends nothing the server would
  // refuse. `mutate` is fire-and-forget, so it does nothing; `mutateAsync`
  // rejects so an awaiting caller does not hang.
  return {
    ...mutation,
    mutate: () => undefined,
    mutateAsync: () => Promise.reject(new AdminViewOnlyError(itemId)),
  };
}

export interface AdminWriteAccess {
  readonly itemId: string;
  /** The document's answer; `undefined` while it is unknown. */
  readonly access: AdminItemAccess | undefined;
  /** False only when the document is known and gives less than `write`. */
  readonly writable: boolean;
  /** The mutation unchanged when writable, otherwise one that sends nothing. */
  readonly guard: <M extends GuardableMutation>(mutation: M) => M;
}

/** The hook form of the wrapper, for a page's state hook: wrap each mutation in `guard`. */
export function useAdminWritable(itemId: string): AdminWriteAccess {
  const access = useAdminItemAccess(itemId);
  const writable = writableAt(access);
  return {
    itemId,
    access,
    writable,
    guard: (mutation) => guardMutation(mutation, itemId, writable),
  };
}

/** What `AdminWritable` hands the controls under it. */
export interface AdminWriteScope {
  readonly itemId: string;
  readonly access: AdminItemAccess | undefined;
}

export const AdminWriteScopeContext = createContext<AdminWriteScope | null>(null);

export interface AdminWriteControlState {
  readonly disabled: boolean;
  /** Why the control is disabled, for its tooltip; `undefined` when it is enabled. */
  readonly reason: string | undefined;
}

/**
 * The state of one mutation control. Outside an `AdminWritable` (a page not
 * converted yet, story R3.4b) the control is enabled, as it was before.
 */
export function useAdminWriteControl(): AdminWriteControlState {
  const scope = useContext(AdminWriteScopeContext);
  if (scope === null || writableAt(scope.access)) return { disabled: false, reason: undefined };
  return {
    disabled: true,
    reason: t('pages.admin.viewOnly.controlReason', 'View only: your roles can see this page but not change it.'),
  };
}

/** A role as the console names it: the built-in display name, else the role's own name. */
export function consoleRoleName(role: { readonly name: string; readonly display_name?: string | undefined }): string {
  return role.display_name ?? role.name;
}
