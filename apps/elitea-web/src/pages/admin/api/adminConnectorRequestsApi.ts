/**
 * REST client for `Admin › Connector requests` (ADR-0044 decision 1, #1501):
 * the approval queue of every project's connector requests. The operations
 * come from orval (`listAdminConnectorRequests`, `approveConnectorRequest`,
 * `rejectConnectorRequest`); this module holds the envelope peel, the query
 * keys and the refusal sentence.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  approveConnectorRequest,
  listAdminConnectorRequests,
  rejectConnectorRequest,
} from '@/shared/api/generated/admin/admin';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { ConnectorApprovalOutcome, ConnectorRequest, ConnectorRequestState } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

const KEY = ['admin', 'connector-requests'] as const;

function requestRows(response: unknown): ConnectorRequest[] {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null) {
    const rows = (body as { rows?: unknown }).rows;
    if (Array.isArray(rows)) return rows as ConnectorRequest[];
  }
  throw new Error('connector requests: the server answered with an unrecognised body');
}

/** The queue, of one state or every state (`''`). */
export function useAdminConnectorRequests(state: ConnectorRequestState | '') {
  return useQuery({
    queryKey: [...KEY, state],
    queryFn: async () => requestRows(await listAdminConnectorRequests(state === '' ? {} : { state })),
  });
}

export interface ConnectorDecision {
  readonly id: number;
  readonly approve: boolean;
  readonly reason: string;
}

export function useDecideConnectorRequest() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, approve, reason }: ConnectorDecision): Promise<ConnectorRequest> => {
      if (approve) {
        const outcome = unwrapBody(await approveConnectorRequest(id, reason === '' ? {} : { reason })) as ConnectorApprovalOutcome;
        return outcome.request;
      }
      return unwrapBody(await rejectConnectorRequest(id, { reason })) as ConnectorRequest;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: KEY }),
  });
}

/** The server's sentence for a refused decision (self-approval, a missing attestation, a disabled type…). */
export function describeDecisionError(error: unknown): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const body = error.failure.body;
    const message = typeof body === 'object' && body !== null ? (body as Record<string, unknown>)['message'] : undefined;
    if (typeof message === 'string' && message !== '') return message;
  }
  return t('pages.admin.connectorRequests.decisionError', 'The decision could not be recorded. Try again.');
}
