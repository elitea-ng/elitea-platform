import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  approveAgentChangeRequest,
  createAgentChangeRequest,
  getAgentChangeRequest,
  getAgentRegistry,
  getGetApplicationQueryKey,
  getReleaseGate,
  listAgentChangeRequests,
  overrideAgentChangeRequestGate,
  rejectAgentChangeRequest,
  submitAgentChangeRequest,
  updateAgentRegistry,
  withdrawAgentChangeRequest,
} from '@/shared/api/generated/applications/applications';
import type {
  AgentChangeApprovalOutcome,
  AgentChangeRequest,
  AgentChangeRequestCreate,
  AgentChangeRequestList,
  AgentRegistryRecord,
  AgentRegistryUpdate,
  ReleaseGate,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

/**
 * Agent lifecycle governance (ADR-0040 decisions 1 and 3, #1497): the
 * registry record and the change requests, as the generated client serves
 * them. Every call reads through the envelope (`unwrapBody`): `eliteaFetch`
 * resolves `{data, status, headers}`, and the generated `Promise<T>` is a cast
 * over it.
 */

const KEY = ['agent-lifecycle', 'governance'] as const;

function registryKey(projectId: string | undefined, applicationId: number | undefined) {
  return [...KEY, 'registry', projectId, applicationId] as const;
}

function changeRequestsKey(projectId: string | undefined, applicationId: number | undefined) {
  return [...KEY, 'change-requests', projectId, applicationId] as const;
}

const ready = (projectId: string | undefined, applicationId: number | undefined): boolean =>
  projectId !== undefined && projectId !== '' && applicationId !== undefined && Number.isFinite(applicationId);

/** The agent's registry record: owners, tier, certification. */
export function useAgentRegistry(projectId: string | undefined, applicationId: number | undefined, enabled = true) {
  return useQuery({
    queryKey: registryKey(projectId, applicationId),
    enabled: enabled && ready(projectId, applicationId),
    queryFn: async () =>
      unwrapBody(await getAgentRegistry(projectId ?? '', applicationId ?? 0)) as AgentRegistryRecord,
  });
}

export function useUpdateAgentRegistry(projectId: string | undefined, applicationId: number | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (update: AgentRegistryUpdate) =>
      unwrapBody(await updateAgentRegistry(projectId ?? '', applicationId ?? 0, update)) as AgentRegistryRecord,
    onSuccess: (record) => queryClient.setQueryData(registryKey(projectId, applicationId), record),
  });
}

/** Every change request of the agent, newest first. */
export function useChangeRequests(projectId: string | undefined, applicationId: number | undefined, enabled = true) {
  return useQuery({
    queryKey: changeRequestsKey(projectId, applicationId),
    enabled: enabled && ready(projectId, applicationId),
    queryFn: async () =>
      unwrapBody(await listAgentChangeRequests(projectId ?? '', applicationId ?? 0)) as AgentChangeRequestList,
  });
}

/** One change request; the single read is the one that carries the gate state. */
export function useChangeRequest(
  projectId: string | undefined,
  applicationId: number | undefined,
  changeRequestId: number | undefined,
) {
  return useQuery({
    queryKey: [...changeRequestsKey(projectId, applicationId), 'detail', changeRequestId],
    enabled: ready(projectId, applicationId) && changeRequestId !== undefined,
    queryFn: async () =>
      unwrapBody(
        await getAgentChangeRequest(projectId ?? '', applicationId ?? 0, changeRequestId ?? 0),
      ) as AgentChangeRequest,
  });
}

/** The release gate in full: per-suite outcomes and the regressed cases. */
export function useReleaseGate(
  projectId: string | undefined,
  applicationId: number | undefined,
  gateId: number | null | undefined,
) {
  return useQuery({
    queryKey: [...KEY, 'gate', projectId, applicationId, gateId],
    enabled: ready(projectId, applicationId) && typeof gateId === 'number',
    queryFn: async () =>
      unwrapBody(await getReleaseGate(projectId ?? '', applicationId ?? 0, gateId ?? 0)) as ReleaseGate,
  });
}

/**
 * Creates a change request and submits it, in that order. Two calls because
 * the server keeps a draft state between them; the request dialog has no use
 * for a draft, so a refused submit withdraws it.
 */
export function useRequestChange(projectId: string | undefined, applicationId: number | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: AgentChangeRequestCreate): Promise<AgentChangeRequest> => {
      const created = unwrapBody(
        await createAgentChangeRequest(projectId ?? '', applicationId ?? 0, body),
      ) as AgentChangeRequest;
      try {
        return unwrapBody(
          await submitAgentChangeRequest(projectId ?? '', applicationId ?? 0, created.id),
        ) as AgentChangeRequest;
      } catch (error) {
        // An agent holds one open request. A draft left by a refused submit
        // would make every retry from the dialog answer
        // change_request_open, so withdraw it before reporting the refusal.
        await withdrawAgentChangeRequest(projectId ?? '', applicationId ?? 0, created.id, {
          reason: 'Submit refused',
        }).catch(() => undefined);
        throw error;
      }
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: changeRequestsKey(projectId, applicationId) }),
  });
}

export type ChangeRequestAction =
  | { readonly kind: 'submit'; readonly id: number }
  | { readonly kind: 'approve'; readonly id: number; readonly note: string }
  | { readonly kind: 'reject'; readonly id: number; readonly reason: string }
  | { readonly kind: 'withdraw'; readonly id: number; readonly reason: string }
  | { readonly kind: 'override'; readonly id: number; readonly reason: string };

export interface ChangeRequestActionOutcome {
  readonly changeRequest: AgentChangeRequest;
  /** Set by the final approval of a release candidate, which publishes it. */
  readonly published?: AgentChangeApprovalOutcome['published'];
}

async function runAction(
  projectId: string,
  applicationId: number,
  action: ChangeRequestAction,
): Promise<ChangeRequestActionOutcome> {
  const reason = 'reason' in action && action.reason !== '' ? { reason: action.reason } : {};
  switch (action.kind) {
    case 'submit':
      return { changeRequest: unwrapBody(await submitAgentChangeRequest(projectId, applicationId, action.id)) as AgentChangeRequest };
    case 'approve': {
      const outcome = unwrapBody(
        await approveAgentChangeRequest(projectId, applicationId, action.id, action.note === '' ? {} : { note: action.note }),
      ) as AgentChangeApprovalOutcome;
      return { changeRequest: outcome.change_request, ...(outcome.published ? { published: outcome.published } : {}) };
    }
    case 'reject':
      return { changeRequest: unwrapBody(await rejectAgentChangeRequest(projectId, applicationId, action.id, reason)) as AgentChangeRequest };
    case 'withdraw':
      return { changeRequest: unwrapBody(await withdrawAgentChangeRequest(projectId, applicationId, action.id, reason)) as AgentChangeRequest };
    case 'override':
      return {
        changeRequest: unwrapBody(
          await overrideAgentChangeRequestGate(projectId, applicationId, action.id, { reason: action.reason }),
        ) as AgentChangeRequest,
      };
  }
}

/** Approve, reject, withdraw or override the gate of one change request. */
export function useChangeRequestAction(projectId: string | undefined, applicationId: number | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (action: ChangeRequestAction) => runAction(projectId ?? '', applicationId ?? 0, action),
    onSuccess: (outcome) => {
      void queryClient.invalidateQueries({ queryKey: changeRequestsKey(projectId, applicationId) });
      void queryClient.invalidateQueries({ queryKey: registryKey(projectId, applicationId) });
      // The final approval publishes: the editor's version list shows the status.
      if (outcome.published !== undefined && projectId !== undefined && applicationId !== undefined) {
        void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, applicationId) });
      }
    },
  });
}
