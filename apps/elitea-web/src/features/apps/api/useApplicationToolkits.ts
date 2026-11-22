import { useMemo } from 'react';

import type { ToolkitInstance } from '@/shared/api/generated/model';
import { useListToolkitInstances } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapListPage } from '@/shared/api/unwrap';

import { APPLICATION_CATALOG_TYPES, WIKI_TOOLKIT_TYPES } from '../lib/constants';

import { useSelectedProjectId } from './useSelectedProjectId';

/** The server's page ceiling (`listToolkitInstancesParamsLimitMax`): one request holds every toolkit a project realistically has. */
const LIST_LIMIT = 1000;

/** Every stored spelling of an application type, one `type` value each: the server filters on them exactly (#2189). */
const APPLICATION_TYPES: readonly string[] = [...new Set([...APPLICATION_CATALOG_TYPES, ...WIKI_TOOLKIT_TYPES])];

export interface ApplicationToolkitsState {
  /** The project's toolkit instances whose type is an application type (Wikis, Inventory). */
  readonly applications: readonly ToolkitInstance[];
  readonly isLoading: boolean;
  readonly isError: boolean;
}

/**
 * The project's applications.
 *
 * An application IS a toolkit instance of an application type: creating a
 * Wiki or an Inventory (`/apps/create/:appType`) saves a toolkit, and
 * `GET /elitea_core/applications` lists agents, so it never held them. That
 * is why the Applications tab was empty and `hasApplications` was always
 * `false` for a project that had created one (#2113). The list endpoint
 * filters on the application types itself (`type`, repeated, client contract
 * 1.65), so a project with more toolkits than one page holds still lists every
 * application. A server before 1.65 ignores `type`, so the rows are narrowed
 * here as well.
 */
export function useApplicationToolkits(): ApplicationToolkitsState {
  const projectId = useSelectedProjectId();
  const query = useListToolkitInstances(
    projectId ?? '',
    { limit: LIST_LIMIT, mcp: false, type: [...APPLICATION_TYPES] },
    { query: { enabled: projectId !== undefined } },
  );
  const rows = useMemo(
    () => (query.data === undefined ? [] : unwrapListPage<ToolkitInstance>(query.data, 'listToolkitInstances').rows),
    [query.data],
  );
  const applications = useMemo(() => rows.filter((row) => APPLICATION_TYPES.includes(row.type)), [rows]);

  return { applications, // First load only: a background refetch must not read as "loading" (the Apps redirect waits on it).
    isLoading: query.isLoading, isError: query.isError };
}
