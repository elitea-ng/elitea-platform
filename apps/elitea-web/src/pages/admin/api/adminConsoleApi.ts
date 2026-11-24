/**
 * The console capability document (ADR-0059 decision 6, R3.1b, issue 2611).
 *
 * `GET /admin/console` names the areas and items the caller can open, the
 * level of each, the caller's administration roles and the active elevation.
 * It is the one read the console decides what to show from. The call comes
 * from orval (`getAdminConsoleDocument`); this module holds the envelope
 * peel, the timeout, the selectors and the two refetch triggers.
 *
 * FAILURE MUST NOT READ AS NO ACCESS. The selectors answer `undefined` while
 * the document is unknown (pending, failed, timed out) and `null` only when
 * the document is known and does not open the area or item. A caller that
 * treats `undefined` as "no access" turns a resolver failure into an empty
 * console, which is the defect the document exists to remove.
 *
 * Nothing here reads a permission name: the document carries none.
 */
import { useEffect } from 'react';

import { hashKey, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';

import {
  getAdminConsoleDocument,
  getGetAdminConsoleDocumentQueryKey,
} from '@/shared/api/generated/admin/admin';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type {
  AdminConsoleDocument,
  AdminConsoleElevation,
  AdminConsoleRole,
} from '@/shared/api/generated/model';

const CONSOLE_KEY = getGetAdminConsoleDocumentQueryKey();

/**
 * How long one read may take before the console shows the error state. The
 * resolver answers in milliseconds; a read still open after this is a stuck
 * request, and a spinner that never ends hides that as surely as an empty
 * navigation would.
 */
const CONSOLE_TIMEOUT_MS = 10_000;

/**
 * A page that fires several requests the server refuses gets the refusals in
 * one burst. They all mean the same thing (the caller's access changed), so
 * the first one schedules ONE refetch and the rest of the burst joins it.
 */
const FORBIDDEN_BURST_MS = 250;

type AccessLevel = 'read' | 'write';

export interface AdminConsole {
  /** `pending` until the first answer, `error` when the read failed or timed out. */
  readonly status: 'pending' | 'error' | 'success';
  /** Reads the document again; the error state's retry control calls it. */
  readonly retry: () => void;
  /** The highest level of any item of the area; `null` when none opens; `undefined` while unknown. */
  readonly areaLevel: (areaId: string) => AccessLevel | null | undefined;
  /** The level of the item; `null` when it does not open; `undefined` while unknown. */
  readonly itemLevel: (itemId: string) => AccessLevel | null | undefined;
  /** The caller's administration roles; `undefined` while unknown. */
  readonly roles: readonly AdminConsoleRole[] | undefined;
  /** The active elevation; `null` when there is none; `undefined` while unknown. */
  readonly elevation: AdminConsoleElevation | null | undefined;
}

/** One read, aborted after `CONSOLE_TIMEOUT_MS` or when the query is cancelled. */
async function readConsoleDocument(signal: AbortSignal): Promise<AdminConsoleDocument> {
  const controller = new AbortController();
  const abort = (): void => controller.abort();
  signal.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(abort, CONSOLE_TIMEOUT_MS);
  try {
    const response = await getAdminConsoleDocument({ signal: controller.signal });
    // eliteaFetch rejects every non-2xx, so this is the 200; the check keeps
    // the error bodies of the generated union out of the type.
    if (response.status !== 200) throw new Error(`admin console: unexpected status ${String(response.status)}`);
    return response.data;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener('abort', abort);
  }
}

function levelOfArea(known: AdminConsoleDocument, areaId: string): AccessLevel | null {
  const area = known.areas.find((entry) => entry.id === areaId);
  if (area === undefined) return null;
  return area.items.some((item) => item.level === 'write') ? 'write' : 'read';
}

function levelOfItem(known: AdminConsoleDocument, itemId: string): AccessLevel | null {
  for (const area of known.areas) {
    const item = area.items.find((entry) => entry.id === itemId);
    if (item !== undefined) return item.level;
  }
  return null;
}

/**
 * A refetch that fails after a success keeps the last document: the console
 * shows what it last knew, not an error over a working page. A retry after a
 * failure shows the loading state again until it answers.
 */
function consoleStatus(known: boolean, failed: boolean, fetching: boolean): AdminConsole['status'] {
  if (known) return 'success';
  return failed && !fetching ? 'error' : 'pending';
}

/** The capability document and its selectors. */
export function useAdminConsole(): AdminConsole {
  const query = useQuery({
    queryKey: CONSOLE_KEY,
    queryFn: ({ signal }) => readConsoleDocument(signal),
  });
  const known = query.data;
  return {
    status: consoleStatus(known !== undefined, query.isError, query.isFetching),
    retry: () => {
      void query.refetch();
    },
    areaLevel: (areaId) => (known === undefined ? undefined : levelOfArea(known, areaId)),
    itemLevel: (itemId) => (known === undefined ? undefined : levelOfItem(known, itemId)),
    roles: known?.roles,
    elevation: known === undefined ? undefined : known.elevation,
  };
}

function isForbidden(error: unknown): boolean {
  if (!(error instanceof EliteaApiError)) return false;
  const { failure } = error;
  return (failure.kind === 'http' || failure.kind === 'auth') && failure.status === 403;
}

/** Refetches the document, joining a read already in flight instead of restarting it. */
function refetchConsole(queryClient: QueryClient): void {
  void queryClient.refetchQueries({ queryKey: CONSOLE_KEY, exact: true, type: 'active' }, { cancelRefetch: false });
}

/**
 * The two refetch triggers, mounted once by `AdminLayout`.
 *
 *  - Any admin API answer with status 403, query or mutation: the server
 *    refused something the document said the caller could do, so the
 *    caller's access changed (a role taken away, an elevation ended). All
 *    admin pages read through the admin bundle's one query client, so its
 *    two caches are the one place every such answer passes.
 *  - The window regaining focus. TanStack Query's own focus refetch listens
 *    to `visibilitychange` only, which a window that was merely behind
 *    another window never fires.
 */
export function useAdminConsoleRefresh(): void {
  const queryClient = useQueryClient();
  useEffect(() => {
    const consoleHash = hashKey(CONSOLE_KEY);
    let burst: ReturnType<typeof setTimeout> | undefined;
    const onForbidden = (): void => {
      if (burst !== undefined) return;
      burst = setTimeout(() => {
        burst = undefined;
        refetchConsole(queryClient);
      }, FORBIDDEN_BURST_MS);
    };
    const unsubscribeQueries = queryClient.getQueryCache().subscribe((event) => {
      // The document's own read is left out: a refused read refetching itself would loop.
      if (event.query.queryHash === consoleHash) return;
      if (event.type === 'updated' && event.action.type === 'error' && isForbidden(event.action.error)) onForbidden();
    });
    const unsubscribeMutations = queryClient.getMutationCache().subscribe((event) => {
      if (event.type === 'updated' && event.action.type === 'error' && isForbidden(event.action.error)) onForbidden();
    });
    const onFocus = (): void => refetchConsole(queryClient);
    window.addEventListener('focus', onFocus);
    return () => {
      unsubscribeQueries();
      unsubscribeMutations();
      window.removeEventListener('focus', onFocus);
      if (burst !== undefined) clearTimeout(burst);
    };
  }, [queryClient]);
}
