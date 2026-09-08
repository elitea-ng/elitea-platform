import { useEffect, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application, ApplicationList } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { useRailTagSelection } from '@/shared/ui/EntityRail';
import { useListSearchQuery } from '@/widgets/page-header';

import { sortPipelinesByField, type SortOrder } from './lib/sortPipelinesByField';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { PipelineListPanel, type PipelineListRow } from './ui/PipelineListPanel';

const PAGE_SIZE = 20;

export interface PrivatePipelinesListProps {
  /** Mirrors the baseline's `cardContentType` (`ContentType.PipelineAdmin`/`ContentType.PipelineAll`) — narrowed to a label selector, since this app has no `CardList` card-type-keyed rendering. */
  readonly cardContentType: 'admin' | 'all';
}

/** Same "Untitled" fallback `entities/application`'s `applicationDisplayName` selector encodes — reproduced locally for the same reason `pages/agents/PrivateAgentsList.tsx` (Wave-2 unit A1g) documents: this file works with the GENERATED snake_case `Application` type directly, not the `entities/application` domain type. */
function applicationName(application: Application): string {
  return application.name.trim() !== '' ? application.name : 'Untitled';
}

function toRow(application: Application): PipelineListRow {
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
 * Ported from
 * `apps/elitea-ui/src/pages/Pipelines/PrivatePipelinesList.jsx` — the "my
 * project's own pipelines" tab content. Unlike the sibling `agents` domain's
 * `PrivateAgentsList` (six status-filtered tabs), the baseline's own
 * `Pipelines.jsx` only ever renders this component with a SINGLE status
 * filter, `[CollectionStatus.All]` (verified directly:
 * `Pipelines.jsx:169-197`, both the public-project "Admin" tab and the
 * private-project "All" tab), so this port carries no `statuses` prop at
 * all — every row the project has, unfiltered by status — matching that
 * exact baseline call shape rather than inventing a status axis the
 * original page never exercises.
 *
 * Works directly with the GENERATED (snake_case) `Application`/
 * `ApplicationList` types rather than `entities/application`'s normalised
 * domain type, same `exactOptionalPropertyTypes` mismatch reason
 * `pages/agents/PrivateAgentsList.tsx`/`EditApplication.tsx` (Wave-2 unit
 * A1g) already document in full (confirmed directly via `tsc`, TS2345/
 * TS2379).
 *
 * **Real, disclosed backend gaps** (`agents_type: 'pipeline'` swapped in for
 * `'classic'`, otherwise the identical contract `pages/agents/
 * PrivateAgentsList.tsx` documents):
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
 *    This means the "All"/"Admin" tab BADGE count (`usePipelinesData.ts`'s
 *    `applicationsTotal`, the real, uncapped `wire.total`) can legitimately
 *    read higher than the number of rows this list ever shows — a real,
 *    disclosed UI inconsistency caused by the same spec gap, not a second
 *    bug in this file. TODO(spec gap): add `limit`/`offset` to
 *    `ListApplicationsParams` (the handler already supports them
 *    server-side) so this file can wire real pagination against the
 *    response's own `total`/`page`/`page_size` fields (`ApplicationList`
 *    already carries all three).
 *  - `sort_by`/`sort_order` (declared search params on `/pipelines/$tab`,
 *    `src/routes/_shell/pipelines/$tab.tsx`) are applied client-side via
 *    `sortPipelinesByField`.
 *
 * Drops the baseline's right-rail `RightInfoPanel`/`Categories`/
 * `TrendingAuthors` and its `EmptyStatePage`/tour CTA — no confirmed
 * `shared/ui`/`widgets` port, out of this unit's ownership fence.
 */
export function PrivatePipelinesList({ cardContentType }: PrivatePipelinesListProps): ReactNode {
  const projectId = useSelectedProjectId();
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { tab?: string };
  const search = useSearch({ strict: false }) as SortSearch;
  const sortBy = search.sort_by === 'name' ? 'name' : 'createdAt';
  const sortOrder: SortOrder = search.sort_order === 'asc' ? 'asc' : 'desc';

  // See `pages/agents/PrivateAgentsList.tsx` — the search box lives on the
  // page header and reaches this tab as the route's `query` search param.
  const query = useListSearchQuery();
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const { selectedTags } = useRailTagSelection();

  useEffect(() => {
    setVisibleCount(PAGE_SIZE);
  }, [query]);

  // See `pages/agents/PrivateAgentsList.tsx` — the same server-side tag
  // filter, over the same list endpoint with `agents_type: 'pipeline'`.
  const trimmedQuery = query.trim();
  const tagFilter = selectedTags.join(',');
  const listQuery = useListApplications(
    projectId ?? '',
    {
      agents_type: 'pipeline',
      // `query` is a real server-side substring filter on this endpoint
      // (`ListApplicationsParams.query`, read by
      // `internal/api/v2/applications/handler.go`). Sending it matters
      // because the response is capped at the handler's first 20 rows: a
      // client-side filter alone would narrow one page instead of the project.
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
    const byQuery = rows.filter((row) => matchesQuery(row, query));
    return sortPipelinesByField(byQuery, sortBy, sortOrder, (row, field) =>
      field === 'name' ? applicationName(row) : row.created_at,
    );
  }, [wire, query, sortBy, sortOrder]);

  const visibleRows = filteredSorted.slice(0, visibleCount);
  const hasMore = visibleCount < filteredSorted.length;

  return (
    <Box
      key={cardContentType}
      sx={containerSx}
    >
      <PipelineListPanel
        rows={visibleRows.map(toRow)}
        isLoading={listQuery.isFetching && wire === undefined}
        isError={listQuery.isError}
        errorMessage={t('pages.pipelines.privateList.error', 'Failed to load pipelines.')}
        emptyTitle={
          query
            ? t('pages.pipelines.privateList.emptyFound.title', 'Nothing found.')
            : t('pages.pipelines.privateList.empty.title', 'No pipelines yet')
        }
        emptyDescription={
          query
            ? t('pages.pipelines.privateList.emptyFound.description', 'Create yours now!')
            : t(
                'pages.pipelines.privateList.empty.description',
                'Create your first pipeline to get started. Pipelines chain agents, skills and toolkits into one repeatable flow.',
              )
        }
        onCreate={() => {
          void navigate({ to: '/pipelines/create' });
        }}
        onSelect={(id) => {
          void navigate({ to: '/pipelines/$tab/$agentId', params: { tab: params.tab ?? 'all', agentId: id } });
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
