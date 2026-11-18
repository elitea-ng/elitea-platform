import { useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * Delegation grants (ADR-0044 amendment 1, delegation epic 2156, client contract 1.59:
 * listDelegationGrants, getDelegationGrant, approveDelegationGrant, revokeDelegationGrant,
 * previewDelegationGrant). Attaching a sub-agent, an agent tool or a pipeline child to an agent
 * creates a grant for that edge listing what the callee holds. The app has no agent editor, so it
 * shows an agent's grants and lets an editor approve or revoke one; attaching and the governed
 * change request are on the web and desktop.
 *
 * Everything the server says about a grant is shown as plain text and never interpreted.
 */

export const DELEGATION_CONTRACT = { major: 1, minor: 59 } as const;

export interface DelegationItem {
  /** toolkit, tool, credential, data_class or egress_host. */
  kind: string;
  name: string;
  toolkit: string | null;
}

export interface DelegationGrantView {
  id: number;
  calleeApplicationId: number;
  edgeKind: string;
  /** pending, approved, stale, superseded or revoked. */
  state: string;
  governed: boolean;
  legacy: boolean;
  changeRequestId: number | null;
  items: DelegationItem[];
  /** What the callee held when proposed and the approver left out. */
  omitted: DelegationItem[];
}

export type DelegationFailure = 'offline' | 'not_available' | 'refused' | 'failed';

export class DelegationError extends Error {
  constructor(
    readonly reason: DelegationFailure,
    message: string,
    readonly code?: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = 'DelegationError';
  }
}

export const DELEGATION_COPY = {
  offline: 'Delegations need a connection to your workspace.',
  notAvailable: "This workspace doesn't show delegations in the app yet.",
  load: "Couldn't load the delegations of this agent. Try again.",
  approve: "The delegation wasn't approved. Try again.",
  revoke: "The delegation wasn't revoked. Try again.",
  governed: 'This delegation is decided in a change request. Open it on the web or desktop app to approve it.',
  wrongState: 'This delegation changed in the meantime. Pull to refresh and look again.',
  selfApproval: 'Someone other than the person who proposed this delegation has to approve it.',
  nothingToApprove: 'You hold access to none of the items on this delegation, so there is nothing for you to approve.',
  forbidden: 'You may not review delegations of this agent.',
} as const;

const Id = z.number().int().positive().max(Number.MAX_SAFE_INTEGER);
const Item = z.looseObject({ kind: z.string().max(32), name: z.string().max(500), toolkit: z.string().max(500).nullish() });
const Grant = z.looseObject({
  id: Id,
  callee_application_id: Id,
  edge_kind: z.string().max(32).catch(''),
  state: z.string().max(32),
  governed: z.boolean().catch(false),
  legacy: z.boolean().catch(false),
  change_request_id: Id.nullish().catch(null),
  items: z.array(z.unknown()).nullish(),
  omitted: z.array(z.unknown()).nullish(),
});
const Envelope = z.looseObject({ grants: z.array(z.unknown()).nullish() });

const itemsOf = (raw: readonly unknown[] | null | undefined): DelegationItem[] => {
  const out: DelegationItem[] = [];
  for (const entry of raw ?? []) {
    const item = Item.safeParse(entry);
    if (item.success) out.push({ kind: item.data.kind, name: item.data.name, toolkit: item.data.toolkit ?? null });
  }
  return out;
};

/** One grant as the app shows it, or null when it is malformed (never shown half-read). */
export function grantOf(raw: unknown): DelegationGrantView | null {
  const grant = Grant.safeParse(raw);
  if (!grant.success) return null;
  return {
    id: grant.data.id,
    calleeApplicationId: grant.data.callee_application_id,
    edgeKind: grant.data.edge_kind,
    state: grant.data.state,
    governed: grant.data.governed,
    legacy: grant.data.legacy,
    changeRequestId: grant.data.change_request_id ?? null,
    items: itemsOf(grant.data.items),
    omitted: itemsOf(grant.data.omitted),
  };
}

/** The live grants of an answer: superseded and revoked ones are history, not shown. */
export function grantsOf(data: unknown): DelegationGrantView[] {
  const envelope = Envelope.safeParse(data);
  if (!envelope.success) throw new DelegationError('failed', DELEGATION_COPY.load);
  const out: DelegationGrantView[] = [];
  for (const raw of envelope.data.grants ?? []) {
    const grant = grantOf(raw);
    if (grant && grant.state !== 'superseded' && grant.state !== 'revoked' && !out.some((g) => g.id === grant.id)) out.push(grant);
  }
  return out;
}

/** What the caller sees of a grant's state. */
export function stateLabel(grant: Pick<DelegationGrantView, 'state'>): string {
  if (grant.state === 'approved') return 'Approved';
  if (grant.state === 'stale') return 'Needs re-approval';
  if (grant.state === 'pending') return 'Awaiting approval';
  return grant.state;
}

/** Whether the caller can approve here: pending and not governed (a governed one is decided in its change request). */
export const canApprove = (grant: Pick<DelegationGrantView, 'state' | 'governed'>): boolean => grant.state === 'pending' && !grant.governed;

/** Whether the caller can revoke here: an approved or stale grant, or a pending one that is not governed. */
export const canRevoke = (grant: Pick<DelegationGrantView, 'state' | 'governed'>): boolean =>
  grant.state === 'approved' || grant.state === 'stale' || (grant.state === 'pending' && !grant.governed);

function refusalOf(body: unknown): { message: string; code?: string } | null {
  if (body === null || typeof body !== 'object') return null;
  const { error, code } = body as { error?: unknown; code?: unknown };
  if (typeof error !== 'string' || error.trim() === '') return null;
  return { message: error.trim().slice(0, 500), code: typeof code === 'string' && code.length <= 64 ? code : undefined };
}

/** A non-2xx answer in the app's words (the server's own sentence for anything not mapped). */
function httpFailure(response: Response, body: unknown, fallback: string): DelegationError {
  const refusal = refusalOf(body);
  const status = response.status;
  if (status === 409 && refusal?.code === 'DELEGATION_GOVERNED') return new DelegationError('refused', DELEGATION_COPY.governed, refusal.code, status);
  if (status === 409) return new DelegationError('refused', DELEGATION_COPY.wrongState, refusal?.code, status);
  if (status === 403) {
    const self = refusal !== null && /differ from the proposer/i.test(refusal.message);
    return new DelegationError('refused', self ? DELEGATION_COPY.selfApproval : DELEGATION_COPY.forbidden, refusal?.code, status);
  }
  if (status === 400 && refusal !== null && /nothing is left to approve/i.test(refusal.message)) {
    return new DelegationError('refused', DELEGATION_COPY.nothingToApprove, refusal.code, status);
  }
  if (status === 501) return new DelegationError('not_available', DELEGATION_COPY.notAvailable, undefined, status);
  if (refusal) return new DelegationError('refused', refusal.message, refusal.code, status);
  return new DelegationError('failed', fallback, undefined, status);
}

function thrownFailure(error: unknown, fallback: string): DelegationError {
  if (error instanceof DelegationError) return error;
  if (isApiError(error) && (error.kind === 'network' || error.kind === 'timeout')) return new DelegationError('offline', DELEGATION_COPY.offline);
  return new DelegationError('failed', fallback);
}

/** The text a failure shows: the error's own message. */
export function delegationErrorText(error: unknown, fallback: string = DELEGATION_COPY.load): string {
  return error instanceof DelegationError ? error.message : fallback;
}

const LIST = '/elitea_core/delegation_grants/prompt_lib/{project_id}/{application_id}' as const;
const ONE = '/elitea_core/delegation_grant/prompt_lib/{project_id}/{application_id}/{grant_id}' as const;
const APPROVE = '/elitea_core/delegation_grant/prompt_lib/{project_id}/{application_id}/{grant_id}/approve' as const;

export async function fetchDelegationGrants(
  projectId: number,
  applicationId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<DelegationGrantView[]> {
  try {
    const { data, error, response } = await client.GET(LIST, {
      params: { path: { project_id: String(projectId), application_id: applicationId } },
      signal,
    });
    if (!response.ok) {
      // A server before 1.59 answers the router's 404 with no code.
      if (response.status === 404 && refusalOf(error)?.code === undefined) {
        throw new DelegationError('not_available', DELEGATION_COPY.notAvailable, undefined, 404);
      }
      throw httpFailure(response, error, DELEGATION_COPY.load);
    }
    return grantsOf(data);
  } catch (error) {
    throw thrownFailure(error, DELEGATION_COPY.load);
  }
}

/**
 * Approves a pending grant. With no `items` the server approves everything the caller holds
 * access to and leaves the rest out; a list keeps only those items (as listed on the grant).
 */
export async function approveDelegationGrant(
  projectId: number,
  applicationId: number,
  grantId: number,
  items?: readonly DelegationItem[],
  client: ApiClient = sessionApiClient(),
): Promise<DelegationGrantView> {
  try {
    const { data, error, response } = await client.POST(APPROVE, {
      params: { path: { project_id: String(projectId), application_id: applicationId, grant_id: grantId } },
      ...(items ? { body: { items: items.map((i) => ({ kind: i.kind, name: i.name, ...(i.toolkit ? { toolkit: i.toolkit } : {}) })) } } : {}),
    });
    if (!response.ok) throw httpFailure(response, error, DELEGATION_COPY.approve);
    const grant = grantOf(data);
    if (!grant) throw new DelegationError('failed', DELEGATION_COPY.approve);
    return grant;
  } catch (error) {
    throw thrownFailure(error, DELEGATION_COPY.approve);
  }
}

/** Ends an active grant: the edge is refused again where the caller does not hold what the callee does. */
export async function revokeDelegationGrant(
  projectId: number,
  applicationId: number,
  grantId: number,
  client: ApiClient = sessionApiClient(),
): Promise<void> {
  try {
    const { error, response } = await client.DELETE(ONE, {
      params: { path: { project_id: String(projectId), application_id: applicationId, grant_id: grantId } },
    });
    if (!response.ok) throw httpFailure(response, error, DELEGATION_COPY.revoke);
  } catch (error) {
    throw thrownFailure(error, DELEGATION_COPY.revoke);
  }
}

export function useDelegationGrants(projectId: number | null, applicationId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.delegationGrants(projectId ?? 0, applicationId ?? 0),
    enabled: enabled && projectId !== null && applicationId !== null,
    retry: false,
    gcTime: 0,
    queryFn: ({ signal }) => fetchDelegationGrants(projectId as number, applicationId as number, sessionApiClient(), signal),
  });
}

/** Re-reads the list after an approve or a revoke. */
export function useDelegationRefresh(projectId: number | null, applicationId: number | null) {
  const queryClient = useQueryClient();
  return () => queryClient.invalidateQueries({ queryKey: queryKeys.delegationGrants(projectId ?? 0, applicationId ?? 0) });
}
