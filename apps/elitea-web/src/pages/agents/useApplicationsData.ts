import { useListApplications, useListPublicApplications } from '@/shared/api/generated/applications/applications';
import type { ApplicationList } from '@/shared/api/generated/model';

import { isPublicAgentsProject } from './lib/isPublicAgentsProject';

export interface ApplicationsTabTotals {
  readonly latestTotal: number | undefined;
  readonly myLikedTotal: number | undefined;
  readonly trendingTotal: number | undefined;
  readonly applicationsTotal: number | undefined;
  readonly draftTotal: number | undefined;
  readonly publishedTotal: number | undefined;
  readonly moderationTotal: number | undefined;
  readonly approvalTotal: number | undefined;
  readonly rejectedTotal: number | undefined;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/useApplicationsData.js`
 * — the tab-badge counts `useApplicationTabs.jsx` renders next to each tab
 * label. Rebuilt against this app's generated TanStack Query client instead
 * of RTK Query's `useTotal*Query` family (which has no generated
 * equivalent — see `entities/application-form`'s own doc comments for the
 * same RTKQ->TanStack Query migration convention).
 *
 * **Real, disclosed backend gaps — every one of the five PRIVATE per-status
 * counts and both `myLikedTotal`/`trendingTotal` return `undefined`, not an
 * invented number:**
 *  - `ListApplicationsParams` (`shared/api/generated/model/
 *    listApplicationsParams.zod.ts`) has exactly `query`/`tags`/`folder_id`/
 *    `agents_type` — no `statuses` field. The Go handler behind it
 *    (`internal/api/v2/applications/handler.go:71-107`) never reads a
 *    status filter either. There is therefore no server-side way to ask
 *    "how many of my applications are Draft/Published/On Moderation/
 *    Awaiting Approval/Rejected" — `draftTotal`/`publishedTotal`/
 *    `moderationTotal`/`approvalTotal`/`rejectedTotal` are `undefined`
 *    (PrivateAgentsList, this unit, computes the numerator for whichever
 *    ONE status it is currently showing by filtering its own already-
 *    fetched page client-side — see that file — but this hook does not
 *    replicate that five times over just to populate tab badges).
 *  - `ListPublicApplicationsParams` has exactly one field, `category`
 *    (`shared/api/generated/model/listPublicApplicationsParams.zod.ts`;
 *    handler: `internal/api/v2/eliteacore/handler.go:1251-1317`). There is
 *    no `my_liked` boolean and no `trend_start_period` anywhere on this
 *    endpoint's contract — `myLikedTotal`/`trendingTotal` are `undefined`
 *    for the same reason. `latestTotal` IS real: the "Latest" tab has no
 *    functional filter beyond what this endpoint already can't apply
 *    (query/tags), so its total is exactly `listPublicApplications({})`'s
 *    `.total` — the same call `Latest.tsx` (this unit) itself makes.
 *  - `applicationsTotal` (the private-project "All" tab / public-project
 *    "Admin" tab count) IS real: `useListApplications(projectId, {
 *    agents_type: 'classic' })`'s `.total`, matching `PrivateAgentsList`'s
 *    own unfiltered fetch.
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
    { query: { enabled: projectId !== undefined && (!isPublicProject || hasAdminPermission) } },
  );
  const applicationsList = applicationsQuery.data?.data as ApplicationList | undefined;

  return {
    latestTotal: isPublicProject ? latestList?.total : undefined,
    myLikedTotal: undefined,
    trendingTotal: undefined,
    applicationsTotal: applicationsList?.total,
    draftTotal: undefined,
    publishedTotal: undefined,
    moderationTotal: undefined,
    approvalTotal: undefined,
    rejectedTotal: undefined,
  };
}
