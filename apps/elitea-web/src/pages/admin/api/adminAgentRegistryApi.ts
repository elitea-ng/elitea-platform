/**
 * REST client for `Admin › Agent registry` (ADR-0040 decision 1, #1497): the
 * cross-project registry of agents, JSON pages for the table and CSV or JSON
 * for an export. `listAdminAgentRegistry` comes from orval; this module holds
 * the envelope peel, the filters and the two downloads.
 */
import { keepPreviousData, useQuery } from '@tanstack/react-query';

import { listAdminAgentRegistry } from '@/shared/api/generated/admin/admin';
import type {
  AgentCertificationState,
  AgentRegistryList,
  AgentRiskTier,
  ListAdminAgentRegistryParams,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { filenameFromContentDisposition, triggerBlobDownload } from '@/shared/lib/download';

import { fetchAllPages } from '../adminCsv';

export const REGISTRY_PAGE_SIZE = 100;

/** The table's filters as typed; blank means "any". */
export interface AgentRegistryFilters {
  readonly tier: AgentRiskTier | '';
  readonly state: AgentCertificationState | '';
  readonly ownerId: string;
  readonly projectId: string;
}

export const EMPTY_REGISTRY_FILTERS: AgentRegistryFilters = { tier: '', state: '', ownerId: '', projectId: '' };

function positiveId(value: string): number | undefined {
  const parsed = Number(value.trim());
  return value.trim() !== '' && Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
}

/** The query parameters of a filter set; an id that is not a positive integer is left out. */
function registryParams(filters: AgentRegistryFilters): ListAdminAgentRegistryParams {
  const ownerId = positiveId(filters.ownerId);
  const projectId = positiveId(filters.projectId);
  return {
    ...(filters.tier === '' ? {} : { tier: filters.tier }),
    ...(filters.state === '' ? {} : { state: filters.state }),
    ...(ownerId === undefined ? {} : { owner_id: ownerId }),
    ...(projectId === undefined ? {} : { project_id: projectId }),
  };
}

function registryBody(response: unknown): AgentRegistryList {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null && Array.isArray((body as { rows?: unknown }).rows)) {
    return body as AgentRegistryList;
  }
  throw new Error('agent registry: the server answered with an unrecognised body');
}

export function useAdminAgentRegistry(filters: AgentRegistryFilters, offset: number) {
  return useQuery({
    queryKey: ['admin', 'agent-registry', filters, offset],
    queryFn: async () =>
      registryBody(
        await listAdminAgentRegistry({ ...registryParams(filters), limit: REGISTRY_PAGE_SIZE, offset, format: 'json' }),
      ),
    placeholderData: keepPreviousData,
  });
}

/**
 * Downloads the CSV the server renders (`format=csv`, every matching row up
 * to its export cap). The text is saved as it arrives, under the server's
 * filename when it names one.
 */
export async function downloadRegistryCsv(filters: AgentRegistryFilters): Promise<void> {
  const response = (await listAdminAgentRegistry({ ...registryParams(filters), format: 'csv' })) as {
    readonly data: unknown;
    readonly headers?: Headers;
  };
  const text = typeof response.data === 'string' ? response.data : '';
  const filename = filenameFromContentDisposition(
    response.headers?.get('content-disposition') ?? null,
    'agent-registry.csv',
  );
  triggerBlobDownload(new Blob([text], { type: 'text/csv;charset=utf-8' }), filename);
}

/** Downloads every matching row as one JSON document, walking the pages. */
export async function downloadRegistryJson(filters: AgentRegistryFilters): Promise<{ readonly truncated: boolean }> {
  const all = await fetchAllPages(async (limit, offset) =>
    registryBody(await listAdminAgentRegistry({ ...registryParams(filters), limit, offset, format: 'json' })),
  );
  const document = { total: all.rows.length, rows: all.rows };
  triggerBlobDownload(
    new Blob([JSON.stringify(document, null, 2)], { type: 'application/json' }),
    'agent-registry.json',
  );
  return { truncated: all.truncated };
}
