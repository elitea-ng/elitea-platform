/**
 * REST client for `Admin › Credential inventory` and `Admin › Permission
 * trimming` (ADR-0044 decision 5, #1501). The orval operations come from the
 * spec; this module holds the envelope peel, the filters and the two
 * one-click actions (revoke a credential, remove an unused permission), each
 * audited server-side.
 */
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  listAdminPermissionTrimming,
  listCredentialInventory,
  removeAdminPermissionTrimmingItem,
  revokeInventoryCredential,
} from '@/shared/api/generated/admin/admin';
import type {
  CredentialInventoryList,
  CredentialKind,
  CredentialRevokeRequest,
  CredentialRevokeResult,
  ListAdminPermissionTrimmingParams,
  ListCredentialInventoryParams,
  PermissionTrimmingRemoveRequest,
  PermissionTrimmingRemoveResult,
  PermissionTrimmingReport,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

export const INVENTORY_PAGE_SIZE = 100;

/** The inventory filters as typed; blank means "any". */
export interface InventoryFilters {
  readonly kind: CredentialKind | '';
  readonly projectId: string;
  readonly stale: 'any' | 'stale' | 'fresh';
}

export const EMPTY_INVENTORY_FILTERS: InventoryFilters = {
  kind: '',
  projectId: '',
  stale: 'any',
};

function positiveId(value: string): number | undefined {
  const parsed = Number(value.trim());
  return value.trim() !== '' && Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
}

/** The query parameters of a filter set; a project id that is not a positive integer is left out. */
function inventoryParams(filters: InventoryFilters): ListCredentialInventoryParams {
  const projectId = positiveId(filters.projectId);
  return {
    ...(filters.kind === '' ? {} : { kind: filters.kind }),
    ...(projectId === undefined ? {} : { project_id: projectId }),
    ...(filters.stale === 'any' ? {} : { stale: filters.stale === 'stale' }),
  };
}

function inventoryBody(response: unknown): CredentialInventoryList {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null && Array.isArray((body as { rows?: unknown }).rows)) {
    return body as CredentialInventoryList;
  }
  throw new Error('credential inventory: the server answered with an unrecognised body');
}

const INVENTORY_KEY = ['admin', 'credential-inventory'] as const;
const TRIMMING_KEY = ['admin', 'permission-trimming'] as const;

export function useCredentialInventory(filters: InventoryFilters, offset: number) {
  return useQuery({
    queryKey: [...INVENTORY_KEY, filters, offset],
    queryFn: async () =>
      inventoryBody(
        await listCredentialInventory({
          ...inventoryParams(filters),
          limit: INVENTORY_PAGE_SIZE,
          offset,
        }),
      ),
    placeholderData: keepPreviousData,
  });
}

export function useRevokeCredential() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: CredentialRevokeRequest) =>
      unwrapBody(await revokeInventoryCredential(body)) as CredentialRevokeResult,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: INVENTORY_KEY }),
  });
}

/** The trimming filters as typed; blank means "any" (the window: the policy's). */
export interface AdminTrimmingFilters {
  readonly projectId: string;
  readonly windowDays: string;
  readonly candidatesOnly: boolean;
}

export const EMPTY_TRIMMING_FILTERS: AdminTrimmingFilters = {
  projectId: '',
  windowDays: '',
  candidatesOnly: true,
};

function trimmingParams(filters: AdminTrimmingFilters): ListAdminPermissionTrimmingParams {
  const projectId = positiveId(filters.projectId);
  const window = positiveId(filters.windowDays);
  return {
    ...(projectId === undefined ? {} : { project_id: projectId }),
    ...(window === undefined || window > 365 ? {} : { window_days: window }),
    candidates_only: filters.candidatesOnly,
  };
}

function reportBody(response: unknown): PermissionTrimmingReport {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null && Array.isArray((body as { items?: unknown }).items)) {
    return body as PermissionTrimmingReport;
  }
  throw new Error('permission trimming: the server answered with an unrecognised body');
}

export function useAdminPermissionTrimming(filters: AdminTrimmingFilters) {
  return useQuery({
    queryKey: [...TRIMMING_KEY, filters],
    queryFn: async () => reportBody(await listAdminPermissionTrimming(trimmingParams(filters))),
    placeholderData: keepPreviousData,
  });
}

export function useRemoveAdminTrimmingItem() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PermissionTrimmingRemoveRequest) =>
      unwrapBody(await removeAdminPermissionTrimmingItem(body)) as PermissionTrimmingRemoveResult,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: TRIMMING_KEY }),
  });
}
