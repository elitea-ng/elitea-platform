import { useMemo } from 'react';

import { normaliseAppPage } from '@/entities/app';
import { useListApplications, useListPublicApplications } from '@/shared/api/generated/applications/applications';
import type { ApplicationList } from '@/shared/api/generated/model';

import { isPublicPipelinesProject } from './lib/isPublicPipelinesProject';

export interface PipelinesTabTotals {
  readonly latestTotal: number | undefined;
  readonly myLikedTotal: number | undefined;
  readonly trendingTotal: number | undefined;
  readonly applicationsTotal: number | undefined;
}

/**
 * Supporting hook for `Pipelines.tsx`'s (this unit's own owned file) tab
 * badge counts — the pipelines-domain sibling of `pages/agents/
 * useApplicationsData.ts` (Wave-2 unit A1g), not itself in this unit's
 * old-app-file list (`pages/Pipelines/Pipelines.jsx` reads these totals
 * inline via four separate `useTotal*Query` RTK-Query hooks,
 * `Pipelines.jsx:76-119`) but split out here for the same reason A1g's own
 * file documents: keeping `Pipelines.tsx` itself under the §3.5 line/
 * complexity budget.
 *
 * **Real, disclosed backend gaps, mirroring `useApplicationsData.ts`'s own
 * citation trail (agents_type: 'pipeline' swapped in for 'classic'):**
 *  - `ListPublicApplicationsParams` has no `agents_type` field — but unlike
 *    the agents sibling, this hook filters the fetched rows client-side by
 *    `agent_type === 'pipeline'` (the field IS present on the response,
 *    `PublicApplicationSummary.agent_type` — see `Latest.tsx`'s own doc
 *    comment for the full citation), so `latestTotal` is a real, honestly
 *    computed pipeline-only count, not the endpoint's raw unfiltered total.
 *  - `myLikedTotal`/`trendingTotal` are `undefined` — no `my_liked`/
 *    `trend_start_period` filter exists on this endpoint's contract at all
 *    (see `MyLiked.tsx`/`Trending.tsx`, this unit).
 *  - `applicationsTotal` (the private-project "All" tab / public-project
 *    "Admin" tab count) IS real: `useListApplications(projectId, {
 *    agents_type: 'pipeline' }).total`, matching `PrivatePipelinesList`'s
 *    own unfiltered-by-status fetch.
 */
export function usePipelinesData(projectId: string | undefined, hasAdminPermission: boolean): PipelinesTabTotals {
  const isPublicProject = isPublicPipelinesProject(projectId);

  const latestQuery = useListPublicApplications(
    {},
    { query: { enabled: projectId !== undefined && isPublicProject } },
  );
  // A resolved envelope is the 200 (eliteaFetch throws otherwise); the status
  // check selects it from the union, which names the native-client 400/426.
  const latestWireEnvelope = latestQuery.data;
  const latestWire = latestWireEnvelope?.status === 200 ? latestWireEnvelope.data : undefined;
  const latestTotal = useMemo(() => {
    if (latestWire === undefined) return undefined;
    return normaliseAppPage(latestWire).rows.filter((app) => app.agentType === 'pipeline').length;
  }, [latestWire]);

  const applicationsQuery = useListApplications(
    projectId ?? '',
    { agents_type: 'pipeline' },
    { query: { enabled: projectId !== undefined && (!isPublicProject || hasAdminPermission) } },
  );
  const applicationsList = applicationsQuery.data?.data as ApplicationList | undefined;

  return {
    latestTotal: isPublicProject ? latestTotal : undefined,
    myLikedTotal: undefined,
    trendingTotal: undefined,
    applicationsTotal: applicationsList?.total,
  };
}
