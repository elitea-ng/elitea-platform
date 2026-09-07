import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application, ApplicationList } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { useRailTagSelection } from '@/shared/ui/EntityRail';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import { sortApplicationsByField, type SortOrder } from './lib/sortApplicationsByField';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { ApplicationListPanel, type ApplicationListRow } from './ui/ApplicationListPanel';

const PAGE_SIZE = 20;

export interface PrivateAgentsListProps {
  /** `undefined` (the "All"/public-Admin tab) shows every status; otherwise only rows whose `status` is in this list. */
  readonly statuses: readonly string[] | undefined;
  /** Empty-state copy selector — mirrors the baseline's `cardContentType`-keyed `EmptyListPlaceHolder` branches (`PrivateAgentsList.jsx:40-63`), narrowed to the one axis (status) that still has content to differ on once `query`-vs-not is handled by `ApplicationListPanel`'s single empty state. */
  readonly cardContentType: string;
}

/** Same "Untitled" fallback `entities/application`'s `applicationDisplayName` selector encodes — reproduced locally because this file works with the GENERATED snake_case `Application` type directly (see this file's own doc comment for why), not the entities/application domain type that selector is typed against. */
function applicationName(application: Application): string {
  return application.name.trim() !== '' ? application.name : 'Untitled';
}

function toRow(application: Application): ApplicationListRow {
  return {
    id: application.id,
    name: applicationName(application),
    description: application.description ?? '',
    authors: (application.authors ?? []).map((author) => ({ id: author.id, name: author.name })),
    tags: application.tags ?? [],
    createdAt: application.created_at,
  };
}

function matchesQuery(application: Application, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (needle === '') return true;
  return application.name.toLowerCase().includes(needle);
}

interface SortSearch {
  readonly sort_by?: string;
  readonly sort_order?: string;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/PrivateAgentsList.jsx`
 * — the "my project's own applications, filtered by status" tab content,
 * rendered once per status by `useApplicationTabs.tsx` (this unit).
 *
 * Works directly with the GENERATED (snake_case) `Application`/
 * `ApplicationList` types rather than `entities/application`'s
 * `normaliseApplicationPage`/`Application` domain type — same choice (and
 * same real, verified reason: an `exactOptionalPropertyTypes` mismatch
 * between the generated zod-derived `field?: T | undefined` shape and
 * `entities/application`'s hand-authored `field?: T` `*Wire` input types,
 * confirmed directly via `tsc`, TS2345) as `EditApplication.tsx`'s own doc
 * comment documents in full.
 *
 * **Real, disclosed backend gaps (see `useApplicationsData.ts`'s doc
 * comment for the full citation trail):**
 *  - `ListApplicationsParams` has no `statuses` field — this fetches the
 *    project's `agents_type: 'classic'` page ONCE (see the cap below) and
 *    filters by `application.status` locally. That field is REAL as of the
 *    lifecycle change: `List` now reports each agent's publish state
 *    (`internal/infra/db/repos/applications.go`), where it previously selected
 *    no status at all, so `row.status` was always absent and every status tab
 *    was empty whatever the user did. See `useApplicationTabs.tsx` for which
 *    of the six tabs the server can fill and which it still cannot.
 *  - **Silent 20-row cap, honestly disclosed here:** `ListApplicationsParams`
 *    (the ORVAL-generated request-param type, `shared/api/generated/model/
 *    listApplicationsParams.zod.ts`) has no `limit`/`offset` fields. The Go
 *    handler this call reaches (`internal/api/v2/applications/handler.go:
 *    74-83`) genuinely DOES read real `limit`/`offset` off the raw query
 *    string and defaults `limit` to 20 whenever it is absent/invalid — so
 *    every fetch here is silently capped to the backend's first 20 rows,
 *    NOT "the project's full, unfiltered page" (the previous, now-corrected,
 *    claim this comment made). Because the generated TYPE does not declare
 *    `limit`/`offset` as request params (an OpenAPI spec gap, not a
 *    call-site oversight — `internal/api/v2/applications/handler.go`'s
 *    request-reading code is simply ahead of its own spec'd/generated
 *    contract), there is no type-safe way from this file to ask for more
 *    than that first page. **"Load more" is therefore a dead no-op**:
 *    `visibleCount` starts at `PAGE_SIZE` (20) and the fetched array can
 *    never exceed 20 rows either, so `hasMore` (`visibleCount <
 *    filteredSorted.length`) is never `true` and the button never renders.
 *    TODO(spec gap): add `limit`/`offset` to `ListApplicationsParams` (the
 *    handler already supports them server-side) so this file can wire real
 *    pagination against the response's own `total`/`page`/`page_size`
 *    fields (`ApplicationList` already carries all three).
 *  - `sort_by`/`sort_order` (declared search params on `/agents/$tab`,
 *    `src/routes/_shell/agents/$tab.tsx`) are applied client-side via
 *    `sortApplicationsByField` for the same reason (no equivalent request
 *    param on `ListApplicationsParams`).
 *
 * **Search IS wired to the real backend param**, unlike the two gaps above:
 * `ListApplicationsParams.query` (`listApplicationsParams.zod.ts`) is a real,
 * declared, server-side substring-search param the Go handler reads
 * (`handler.go:85-89`, `r.URL.Query().Get("query")`) — this file passes the
 * debounced search box value through as that param, so search results are no
 * longer limited to whatever happened to be in the first capped page; the
 * local `matchesQuery` filter stays as a client-side belt-and-suspenders
 * pass over whatever the server already filtered, not the only filter.
 *
 * Drops the baseline's right-rail `RightInfoPanel`/`Categories`/
 * `TrendingAuthors` (no confirmed `shared/ui`/`widgets` port, out of this
 * unit's ownership fence — same disclosed scope reduction `pages/
 * credentials/CredentialsList.tsx` already established) and its
 * `EmptyStatePage`/tour CTA for the zero-applications-at-all case (same
 * reason).
 */
export function PrivateAgentsList({ statuses, cardContentType }: PrivateAgentsListProps): ReactNode {
  const projectId = useSelectedProjectId();
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { tab?: string };
  const search = useSearch({ strict: false }) as SortSearch;
  const sortBy = search.sort_by === 'name' ? 'name' : 'createdAt';
  const sortOrder: SortOrder = search.sort_order === 'asc' ? 'asc' : 'desc';

  const [query, setQuery] = useState('');
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const { selectedTags } = useRailTagSelection();

  const trimmedQuery = query.trim();
  // The rail's tag selection goes to the SERVER, as the `tags` request param
  // the handler always read (`internal/api/v2/applications/handler.go`). The
  // repository query behind it ignored the param and left every row's `tags`
  // empty, so this page disclosed the rail as decorative. Both halves are
  // real now (issue 841), and the filter is server-side because the list is
  // capped at the backend's first page: a client-side filter would narrow one
  // page instead of the project.
  const tagFilter = selectedTags.join(',');
  const listQuery = useListApplications(
    projectId ?? '',
    {
      agents_type: 'classic',
      ...(trimmedQuery === '' ? {} : { query: trimmedQuery }),
      ...(tagFilter === '' ? {} : { tags: tagFilter }),
    },
    { query: { enabled: projectId !== undefined } },
  );
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract).
  const wire = listQuery.data?.data as ApplicationList | undefined;

  const filteredSorted = useMemo(() => {
    const rows = wire?.rows ?? [];
    const byStatus = statuses === undefined ? rows : rows.filter((row) => statuses.includes(row.status ?? ''));
    const byQuery = byStatus.filter((row) => matchesQuery(row, query));
    return sortApplicationsByField(byQuery, sortBy, sortOrder, (row, field) =>
      field === 'name' ? applicationName(row) : row.created_at,
    );
  }, [wire, statuses, query, sortBy, sortOrder]);

  const visibleRows = filteredSorted.slice(0, visibleCount);
  const hasMore = visibleCount < filteredSorted.length;

  return (
    <Box
      key={cardContentType}
      sx={containerSx}
    >
      <SimpleSearchBar
        value={query}
        onChange={(next) => {
          setQuery(next);
          setVisibleCount(PAGE_SIZE);
        }}
        placeholder={t('pages.agents.privateList.search', 'Search')}
      />
      <ApplicationListPanel
        rows={visibleRows.map(toRow)}
        isLoading={listQuery.isFetching && wire === undefined}
        isError={listQuery.isError}
        errorMessage={t('pages.agents.privateList.error', 'Failed to load applications.')}
        emptyTitle={
          query
            ? t('pages.agents.privateList.emptyFound.title', 'Nothing found.')
            : t('pages.agents.privateList.empty.title', 'No agents yet')
        }
        emptyDescription={
          query
            ? t('pages.agents.privateList.emptyFound.description', 'Create yours now!')
            : t(
                'pages.agents.privateList.empty.description',
                'Create your first agent to get started. Agents combine a prompt, a model and the toolkits they are allowed to call.',
              )
        }
        onCreate={() => {
          void navigate({ to: '/agents/create' });
        }}
        onSelect={(id) => {
          void navigate({ to: '/agents/$tab/$agentId', params: { tab: params.tab ?? 'all', agentId: id } });
        }}
        hasMore={hasMore}
        isLoadingMore={false}
        onLoadMore={() => {
          setVisibleCount((count) => count + PAGE_SIZE);
        }}
      />
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
});
