import { useMemo } from 'react';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application, ApplicationList } from '@/shared/api/generated/model';

import { applicationCatalog } from '../lib/constants';
import { buildCatalogApplication, filterApplicationSchemas } from '../model/catalog';
import type { CatalogApplication } from '../model/types';

import { useApplicationToolkits } from './useApplicationToolkits';
import { useSelectedProjectId } from './useSelectedProjectId';
import { useToolkitTypeSchemas } from './useToolkitTypeSchemas';

/**
 * `isConfigured` source, replacing the baseline's `useListToolkitTypesQuery
 * ({ params: { application: true } })` (`useApplicationCatalogState.hooks.js:26-29`,
 * `api/toolkits.js:487-497` `toolkitTypes` RTK endpoint).
 *
 * **Backend-capability gap, documented rather than silently dropped:** that
 * baseline call hits the SAME URL as the schema catalogue
 * (`/elitea_core/toolkits/prompt_lib/{projectId}`), relying on the OLD
 * Python backend branching on the `application=true` query param to return
 * a DIFFERENT shape (a `{rows: string[]}` list of configured type keys)
 * than the bare schema-map call. The Go handler behind this route
 * (`internal/api/v2/toolkits/handler.go:231-233`, per the generated
 * client's own `NOTE(W2)`) is `ListTypeSchemas` — it always returns the
 * type→schema map, with no query-param-driven branch to a configured-types
 * list. That baseline call is therefore not portable at all against the Go
 * stack; there is no generated (or hand-registrable, per R-A5) endpoint
 * that answers "which toolkit types already have a configured instance in
 * this project" directly.
 *
 * `useListApplications` (`/elitea_core/applications/prompt_lib/{projectId}`,
 * unit S4/W2, `internal/api/v2/applications/handler.go:71-107`) is the
 * closest AVAILABLE substitute: every configured toolkit instance is an
 * `Application` row, so this hook makes ONE call and derives the
 * configured-type set client-side by checking both `type` and `agent_type`
 * on every returned row (the OpenAPI schema does not pin down which of the
 * two field actually carries a toolkit-catalogue key like `"inventory"`,
 * and checking both is a safe superset, never a false negative either way).
 *
 * **Correction:** an earlier version of this comment claimed
 * `ListApplicationsParams.agents_type` could filter that one call down to a
 * single catalogue type (e.g. `agents_type: "inventory"`), which would have
 * sidestepped the pagination gap below entirely. That claim does not survive
 * reading the repository method the handler calls into
 * (`internal/infra/db/repos/applications.go:30-46`): `agents_type` is a
 * two-valued classic/pipeline discriminator over
 * `application_versions.agent_type` (`"pipeline"` vs. everything else, which
 * collapses to `"classic"`) — it has no branch for arbitrary catalogue type
 * strings at all. Passing a catalogue type like `"wikis_Wikis"` as
 * `agents_type` does not narrow the result set to that type; the switch
 * falls through to its unfiltered default branch instead. There is no
 * request param on this endpoint that filters by the `type`/`agent_type`
 * VALUE this catalogue keys on (only by the classic/pipeline AXIS), so a
 * one-filtered-request-per-entry design was never actually available here —
 * independent of the separate rules-of-hooks concern a variable-length
 * catalogue would raise (see `useModerationRequests.ts`'s `useQueries` for
 * how this same file's sibling hook handles that concern when it DOES apply).
 *
 * **Known limitation, real and NOT fixable from this file:** the Go
 * handler's `ApplicationList` response is paginated (`page`/`page_size`/
 * `total_pages`), and the generated `ListApplicationsParams` request type
 * (`shared/api/generated/model/listApplicationsParams.zod.ts`) exposes
 * exactly `query`/`tags`/`folder_id`/`agents_type` — no `page`, `limit`, or
 * `offset`. The handler itself DOES read real `limit`/`offset` off the raw
 * query string with its own comment "UI sends limit/offset; convert to
 * page/pageSize" (`internal/api/v2/applications/handler.go:71-79`,
 * defaulting `limit` to 20 whenever absent/invalid) — so the contract the
 * generated client was built from is simply incomplete for this operation,
 * not merely unfiltered. This hook (and every other `useListApplications`
 * call site — see `pages/agents/useApplicationsData.ts` and
 * `pages/agents/PrivateAgentsList.tsx`'s own matching disclosure of the
 * identical gap) is therefore stuck on the backend's default first page: a
 * project with more configured applications than that default page holds
 * can have an already-configured type missed if none of its instances land
 * on it. Reaching into the URL by hand with the undocumented `limit`/
 * `offset` names (bypassing the typed `ListApplicationsParams` contract)
 * would "fix" this file in isolation but was deliberately rejected — same
 * call the `PrivateAgentsList.tsx` precedent made — because it trades a
 * disclosed, honestly-tested gap for an undisclosed dependency on backend
 * behavior the generated contract does not promise.
 *
 * TODO(spec gap, out of this cluster's scope — `src/features/apps/` cannot
 * fix this on its own): add `limit`/`offset` (or `page`/`page_size`) to this
 * operation in the OpenAPI spec the client is generated from, then
 * regenerate `ListApplicationsParams`. Once that lands, this hook should
 * fetch every page (`ApplicationList.total_pages`) via `useQueries` — the
 * same pattern `useModerationRequests.ts` already uses for a fixed-length
 * array of queries — and merge every page's rows before deriving
 * `configuredTypes`, matching the baseline endpoint's complete-set guarantee
 * exactly. This file's own test suite (`useApplicationCatalog.test.tsx`,
 * the "beyond the first page" case) pins today's documented limit so that
 * fix is verifiably an improvement, not a silent behavior change.
 */
function deriveConfiguredTypes(applicationList: ApplicationList | undefined): ReadonlySet<string> {
  const configured = new Set<string>();
  if (applicationList === undefined) return configured;
  for (const app of applicationList.rows as readonly Application[]) {
    // Wire field names are snake_case at this layer (the generated model
    // mirrors the Go JSON tags directly — `application.zod.ts`'s `type`/
    // `agent_type` — unlike `entities/application`'s camelCased
    // `Application.agentType`, which this hook does not use).
    if (typeof app.type === 'string') configured.add(app.type);
    if (typeof app.agent_type === 'string') configured.add(app.agent_type);
  }
  return configured;
}

/**
 * Replaces the baseline's `useApplicationCatalogState`
 * (`features/apps/lib/hooks/useApplicationCatalogState.hooks.js`).
 *
 * The baseline's own `theme.palette.icon.fill.default`-coloured
 * `icon_meta` element construction is dropped here — this port's
 * `ApplicationCatalogCard` renders `CatalogApplication.Icon` (the component
 * reference) directly with its own `sx`, so no icon-element needs building
 * in the hook (component vs. fetch separation, spec §3.6).
 */
export function useApplicationCatalog() {
  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas, isFetching: isFetchingSchemas } = useToolkitTypeSchemas(projectId);

  const applicationsQuery = useListApplications(
    projectId ?? '',
    undefined,
    { query: { enabled: projectId !== undefined } },
  );
  // `.data.data`'s declared type includes the error-envelope variant —
  // never actually reachable here since `eliteaFetch` throws instead of
  // resolving with it (mutator.ts's §3.6 unwrap contract).
  const applicationList = applicationsQuery.data?.data as ApplicationList | undefined;

  const applicationSchemas = useMemo(
    () => filterApplicationSchemas(toolkitTypeSchemas),
    [toolkitTypeSchemas],
  );
  // A Wiki or Inventory is a toolkit instance, which the applications list never holds (#2113).
  const { applications: applicationToolkits, isLoading: isLoadingToolkits } = useApplicationToolkits();
  const configuredTypes = useMemo(() => {
    const configured = new Set(deriveConfiguredTypes(applicationList));
    for (const toolkit of applicationToolkits) configured.add(toolkit.type);
    return configured;
  }, [applicationList, applicationToolkits]);

  const applications: readonly CatalogApplication[] = useMemo(
    () => applicationCatalog().map((entry) => buildCatalogApplication(entry, applicationSchemas, configuredTypes)),
    [applicationSchemas, configuredTypes],
  );

  return {
    applications,
    isLoading: isFetchingSchemas || applicationsQuery.isFetching || isLoadingToolkits,
  };
}
