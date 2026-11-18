import type { QueryClient } from '@tanstack/react-query';

import {
  approveDelegationGrant,
  getGetApplicationQueryOptions,
  getListDelegationGrantsQueryKey,
  listDelegationGrants,
  previewDelegationGrant,
  useListDelegationGrants,
} from '@/shared/api/generated/applications/applications';
import type { ApplicationDetail, DelegationGrant, DelegationItem, DelegationPreview } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { edgeKindFor, toolkitNames } from '../lib/delegationReview';

/**
 * The delegation grant calls of the attach review (ADR-0044 amendment 1,
 * A1.2 rule 2; epic 2156 story S3), over the generated client of S2b.
 *
 * Attaching a sub-agent, an agent tool or a pipeline child already records the
 * PENDING grant of the edge in the attach transaction (S2b), listing what the
 * callee held at that moment. This review therefore runs in two halves around
 * the attach: `loadDelegationReview` reads what the attach will let the callee
 * use (the preview, before anything is written), and `settleDelegation` finds
 * the grant the attach proposed and approves the part the editor chose.
 * Nothing here refuses an attach: a review that cannot be read attaches
 * anyway and leaves the grant pending, which the row then says.
 */

/** The same unwrapping every generated-client caller of this slice uses (eliteaFetch throws instead of resolving the error variant). */
function bodyOf<T>(response: unknown): T {
  return (response as { data: T }).data;
}

export interface DelegationReviewRequest {
  readonly calleeId: number;
  readonly calleeName: string;
  readonly calleeVersionId: number;
  readonly preview: DelegationPreview;
  /** `toolkit:<id>` to the toolkit's name, read from the callee's own version. */
  readonly names: ReadonlyMap<string, string>;
}

interface FetchReviewParams {
  readonly queryClient: QueryClient;
  readonly projectId: string;
  /** The PARENT (caller) application. */
  readonly applicationId: number;
  readonly calleeId: number;
  readonly calleeName: string;
  readonly calleeVersionId: number | undefined;
  readonly edgeKind: string;
}

/**
 * The preview of one edge with the callee's toolkit names. Throws when either
 * read fails. `calleeVersionId` undefined reads the callee's default version.
 */
export async function fetchDelegationReview({ queryClient, projectId, applicationId, calleeId, calleeName, calleeVersionId, edgeKind }: FetchReviewParams): Promise<DelegationReviewRequest | null> {
  const detail = bodyOf<ApplicationDetail>(await queryClient.query(getGetApplicationQueryOptions(projectId, calleeId)));
  const version = detail.version_details;
  const versionId = calleeVersionId ?? (version?.id === undefined ? undefined : Number(version.id));
  if (versionId === undefined) return null;
  const preview = bodyOf<DelegationPreview>(
    await previewDelegationGrant(projectId, applicationId, { callee_application_id: calleeId, callee_version_id: versionId, edge_kind: edgeKind }),
  );
  return {
    calleeId,
    calleeName: preview.callee_name === '' ? calleeName : preview.callee_name,
    calleeVersionId: versionId,
    preview,
    names: toolkitNames(version?.tools),
  };
}

interface LoadReviewParams {
  readonly queryClient: QueryClient;
  readonly projectId: string;
  /** The PARENT (caller) application. */
  readonly applicationId: number;
  readonly candidate: { readonly id: number; readonly name: string };
  readonly parentIsPipeline: boolean;
}

/**
 * What attaching the candidate would let it use, or `null` when there is
 * nothing to review: the callee holds nothing (it needs no grant), the edge
 * already has an approved grant, or the preview cannot be read (the attach
 * then goes on and the pending grant says so on the row).
 */
export async function loadDelegationReview({ queryClient, projectId, applicationId, candidate, parentIsPipeline }: LoadReviewParams): Promise<DelegationReviewRequest | null> {
  try {
    const request = await fetchDelegationReview({
      queryClient,
      projectId,
      applicationId,
      calleeId: candidate.id,
      calleeName: candidate.name,
      calleeVersionId: undefined,
      edgeKind: edgeKindFor(parentIsPipeline),
    });
    if (request === null || request.preview.items.length === 0 || request.preview.existing?.state === 'approved') return null;
    return request;
  } catch {
    return null;
  }
}

export type SettleOutcome =
  | { readonly kind: 'approved' }
  | { readonly kind: 'awaiting' }
  | { readonly kind: 'failed'; readonly message: string };

function failureMessage(error: unknown): string {
  const failure = (error as { failure?: { kind?: string; status?: number; body?: unknown } } | undefined)?.failure;
  const body = failure?.body;
  const text = typeof body === 'object' && body !== null && 'error' in body ? (body as { error?: unknown }).error : undefined;
  if (typeof text === 'string' && text !== '') return text;
  return t('agents.delegation.approveFailed', 'The delegation could not be approved.');
}

function isGovernedRefusal(error: unknown): boolean {
  const body = (error as { failure?: { body?: unknown } } | undefined)?.failure?.body;
  return typeof body === 'object' && body !== null && (body as { code?: unknown }).code === 'DELEGATION_GOVERNED';
}

/** The grant the attach proposed for the callee: the edge's one open grant. */
function pendingGrantFor(grants: readonly DelegationGrant[], calleeId: number): DelegationGrant | undefined {
  return grants.find((grant) => grant.callee_application_id === calleeId && (grant.state === 'pending' || grant.state === 'stale'));
}

interface SettleParams {
  readonly projectId: string;
  readonly applicationId: number;
  readonly calleeId: number;
  /** The items to keep, or `undefined` for everything the approver holds access to. */
  readonly items: readonly DelegationItem[] | undefined;
}

/**
 * After the attach: approve the pending grant of the edge with the items the
 * editor kept. Under the governed profile the grant is already a change
 * request another approver decides, so nothing is approved here.
 */
export async function settleDelegation({ projectId, applicationId, calleeId, items }: SettleParams): Promise<SettleOutcome> {
  try {
    const grants = bodyOf<{ grants?: readonly DelegationGrant[] }>(await listDelegationGrants(projectId, applicationId)).grants ?? [];
    const grant = pendingGrantFor(grants, calleeId);
    if (grant === undefined) return { kind: 'approved' };
    // A governed pending grant is decided in its change request; a governed stale one
    // records its re-approval request when approve is asked (the 409 below).
    if (grant.governed && grant.state === 'pending') return { kind: 'awaiting' };
    await approveDelegationGrant(projectId, applicationId, grant.id, items === undefined ? undefined : { items: [...items] });
    return { kind: 'approved' };
  } catch (error) {
    if (isGovernedRefusal(error)) return { kind: 'awaiting' };
    return { kind: 'failed', message: failureMessage(error) };
  }
}

/** The grants of the agent's edges, for the row status. Only read while the agent holds an agent or pipeline tool. */
export function useAgentDelegationGrants(projectId: string | undefined, applicationId: number | undefined, enabled: boolean): readonly DelegationGrant[] {
  const query = useListDelegationGrants(projectId ?? '', applicationId ?? 0, undefined, {
    query: { enabled: enabled && projectId !== undefined && applicationId !== undefined, retry: false },
  });
  return (query.data as { data?: { grants?: readonly DelegationGrant[] } } | undefined)?.data?.grants ?? [];
}

export function invalidateDelegationGrants(queryClient: QueryClient, projectId: string, applicationId: number): Promise<void> {
  return queryClient.invalidateQueries({ queryKey: getListDelegationGrantsQueryKey(projectId, applicationId) });
}
