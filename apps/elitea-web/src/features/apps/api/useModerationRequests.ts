import { useCallback, useMemo, useState } from 'react';

import { useMutation, useQueries, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult } from '@tanstack/react-query';

import {
  createModerationRequest,
  getModerationStatusQueryOptions,
} from '@/shared/api/generated/admin/admin';
import type { ModerationRequestCreate } from '@/shared/api/generated/model';

import { APPLICATION_CATALOG_TYPES, REQUEST_STATUS } from '../lib/constants';
import type { RequestStatus } from '../lib/constants';

import { useSelectedProjectId } from './useSelectedProjectId';

/**
 * DEFECT, fixed here: this file used to hash a catalogue key into a number
 * with FNV-1a (`entityIdForType`) before sending it as `entity_id`.
 *
 * The hash existed because `v2.yaml` typed the `entity_id` path parameter as
 * an integer. That integer type is a leftover from a retired static stub. The
 * generated client therefore demanded a number. The column is a VARCHAR and the handler stores
 * the raw string (`internal/api/v2/moderation/requests.go`), and the legacy
 * EliteaUI sends the key itself ("inventory"). One catalogue entry therefore
 * had two addresses: a request filed here was invisible in the other client
 * and filed a second row, and the admin App Requests queue listed an opaque
 * number. The spec now types the parameter as a string, so the key travels
 * unchanged.
 */

function asRequestStatus(status: unknown): RequestStatus | undefined {
  return status === REQUEST_STATUS.PENDING ||
    status === REQUEST_STATUS.APPROVED ||
    status === REQUEST_STATUS.REJECTED
    ? status
    : undefined;
}

/**
 * The status of the caller's most recent request for this entity, or `NONE`.
 *
 * The endpoint's contract is a `{total, rows}` envelope of the caller's own
 * rows, newest first (`ModerationRequestList` in the spec). Until unit A14 the
 * Go side answered a bare `{"status":"approved"}` from a static stub, which was
 * the only shape this function used to read. Against a real server it found no
 * `status` field and fell through to `NONE`, so the "Pending approval" state on
 * a catalogue card was unreachable and the card kept offering "Request Access"
 * after the request had been filed. Any body that is not that envelope reads
 * as `NONE`.
 */
function statusFromResponse(response: unknown): RequestStatus {
  if (typeof response !== 'object' || response === null) return REQUEST_STATUS.NONE;
  const rows = (response as { rows?: unknown }).rows;
  if (!Array.isArray(rows)) return REQUEST_STATUS.NONE;
  const first: unknown = rows[0];
  if (typeof first !== 'object' || first === null) return REQUEST_STATUS.NONE;
  return asRequestStatus((first as { status?: unknown }).status) ?? REQUEST_STATUS.NONE;
}

interface SubmitModerationRequestVariables {
  readonly projectId: string;
  readonly type: string;
  readonly description: string;
  readonly label: string;
}

/**
 * The generated `createModerationRequest` — the raw POST fetcher, NOT
 * `getCreateModerationRequestQueryOptions`/`useCreateModerationRequest`,
 * which orval generates query-shaped even for this POST (same class of
 * generated-shape mismatch documented at `usePinConversation.hooks.ts`'s
 * `useTogglePinConversation`) — wrapped in a locally-owned `useMutation`.
 *
 * `submitRequest` previously ran this POST through
 * `queryClient.query(getCreateModerationRequestQueryOptions(...))`.
 * That reuses the query machinery's defaults — `retry: 1` and dedup of any
 * in-flight/cached entry sharing the options' `queryKey` within its
 * `staleTime` — against what is a non-idempotent create call: a
 * resubmission with an unchanged reason within that window could silently
 * return the cached prior response with NO new network request, and a
 * transient failure could silently auto-replay the POST once via the query
 * retry. `useMutation` has neither behaviour by default (no cache entry
 * keyed by input, `retry: 0`), so every call this hook makes through it is
 * a true one-shot POST.
 */
function useCreateModerationRequestMutation(): UseMutationResult<
  Awaited<ReturnType<typeof createModerationRequest>>,
  unknown,
  SubmitModerationRequestVariables
> {
  return useMutation({
    mutationFn: ({ projectId, type, description, label }: SubmitModerationRequestVariables) => {
      // Only the two fields a requester owns. The server takes the author
      // from the session and always stores `pending`, and it refuses a
      // non-empty `meta`, so neither is sent.
      const body: ModerationRequestCreate = { issue_type: label, description };
      return createModerationRequest(projectId, type, body);
    },
  });
}

/**
 * Replaces the baseline's `useApplicationRequests`
 * (`features/apps/lib/hooks/useApplicationRequests.hooks.js`). One
 * `useQueries` call replaces the baseline's `Promise.all` + local
 * `moderationData` state — `APPLICATION_CATALOG_TYPES` is a fixed-length
 * module constant, so mapping it to one query-options object per entry is stable
 * across renders (never a variable-length hook call).
 *
 * Until unit A14 the Go endpoints behind this hook were a static stub: every
 * status query resolved to `"approved"` immediately and the POST created
 * nothing, so `getRequestStatus` could never return `PENDING` and the button
 * wrote nowhere. Both are real now
 * (`services/elitea-main/internal/api/v2/moderation/requests.go`), which is
 * what `statusFromResponse` above had to be corrected for.
 *
 * The POST body carries only `issue_type` and `description`, the two fields
 * a requester owns. `status` and `meta` used to be sent as well; the server
 * tolerates them but applies neither, so they are no longer sent.
 */
export function useModerationRequests() {
  const projectId = useSelectedProjectId();
  const queryClient = useQueryClient();
  const [submittingType, setSubmittingType] = useState<string | null>(null);
  const { mutateAsync: createModerationRequestAsync } = useCreateModerationRequestMutation();

  const statusQueries = useQueries({
    queries: APPLICATION_CATALOG_TYPES.map((type) =>
      // The third argument is the operation's query parameters
      // (`issue_type`), NOT the react-query options. Passing the options
      // there silently drops `enabled`. The query then runs with no project.
      getModerationStatusQueryOptions(projectId ?? '', type, undefined, {
        query: { enabled: projectId !== undefined },
      }),
    ),
  });

  const statusByType = useMemo(() => {
    const map = new Map<string, RequestStatus>();
    APPLICATION_CATALOG_TYPES.forEach((type, index) => {
      // `.data.data`'s declared type includes the error-envelope variant —
      // never actually reachable here since `eliteaFetch` throws instead of
      // resolving with it (mutator.ts's §3.6 unwrap contract). It is passed as
      // `unknown` so `statusFromResponse` checks the envelope's shape itself.
      map.set(type, statusFromResponse(statusQueries[index]?.data?.data));
    });
    return map;
  }, [statusQueries]);

  const getRequestStatus = useCallback(
    (type: string): RequestStatus => statusByType.get(type) ?? REQUEST_STATUS.NONE,
    [statusByType],
  );

  const submitRequest = useCallback(
    async (type: string, description: string, label?: string): Promise<void> => {
      if (projectId === undefined) return;
      setSubmittingType(type);
      try {
        await createModerationRequestAsync({
          projectId,
          type,
          description,
          label: label ?? 'Application Access Request',
        });
        await queryClient.invalidateQueries({
          queryKey: getModerationStatusQueryOptions(projectId, type).queryKey,
        });
      } finally {
        setSubmittingType(null);
      }
    },
    [projectId, queryClient, createModerationRequestAsync],
  );

  return {
    getRequestStatus,
    submitRequest,
    isSubmitting: submittingType !== null,
    isFetching: statusQueries.some((query) => query.isFetching),
  };
}
