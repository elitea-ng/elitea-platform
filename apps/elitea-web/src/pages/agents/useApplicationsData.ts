import { useListApplications, useListPublicApplications } from '@/shared/api/generated/applications/applications';
import type { ApplicationList } from '@/shared/api/generated/model';

import { isPublicAgentsProject } from './lib/isPublicAgentsProject';

export interface ApplicationsTabTotals {
  readonly latestTotal: number | undefined;
  readonly myLikedTotal: number | undefined;
  readonly trendingTotal: number | undefined;
  readonly applicationsTotal: number | undefined;
}

/**
 * The PUBLIC project's tab-badge counts (`Latest (3)`, the admin tab's total).
 * A private project has no tabs and so no badge counts: its status and origin
 * counts come with the agents list itself (`ApplicationList.facets`).
 *
 * `myLikedTotal` and `trendingTotal` are `undefined`, not an invented number:
 * `ListPublicApplicationsParams` has exactly one field, `category`
 * (`shared/api/generated/model/listPublicApplicationsParams.zod.ts`), so the
 * public feed cannot be asked for liked or trending rows. `latestTotal` is
 * real, exactly `listPublicApplications({})`'s `.total`, and
 * `applicationsTotal` (the admin tab) is `useListApplications`'s `.total`.
 */
export function useApplicationsData(
  projectId: string | undefined,
  hasAdminPermission: boolean,
): ApplicationsTabTotals {
  const isPublicProject = isPublicAgentsProject(projectId);

  const latestQuery = useListPublicApplications(
    {},
    { query: { enabled: projectId !== undefined && isPublicProject } },
  );
  // A resolved envelope is the 200 (eliteaFetch throws otherwise); the status
  // check selects it from the union, which names the native-client 400/426.
  const latestListEnvelope = latestQuery.data;
  const latestList = latestListEnvelope?.status === 200 ? latestListEnvelope.data : undefined;

  const applicationsQuery = useListApplications(
    projectId ?? '',
    { agents_type: 'classic' },
    { query: { enabled: projectId !== undefined && isPublicProject && hasAdminPermission } },
  );
  const applicationsList = applicationsQuery.data?.data as ApplicationList | undefined;

  return {
    latestTotal: isPublicProject ? latestList?.total : undefined,
    myLikedTotal: undefined,
    trendingTotal: undefined,
    applicationsTotal: applicationsList?.total,
  };
}
