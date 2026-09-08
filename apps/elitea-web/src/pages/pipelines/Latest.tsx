import { useEffect, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate } from '@tanstack/react-router';

import { filterAppsByQuery, normaliseAppPage, type App } from '@/entities/app';
import { useListPublicApplications } from '@/shared/api/generated/applications/applications';
import { t } from '@/shared/i18n';
import { useListSearchQuery } from '@/widgets/page-header';

import { PipelineListPanel, type PipelineListRow } from './ui/PipelineListPanel';

const PAGE_SIZE = 20;

/** `agent_type` value the Go handler writes for a pipeline row (`av.agent_type`, `internal/api/v2/applications/handler.go` — same column `entities/pipeline`'s own doc comment cites for the Application/Pipeline precedent). */
function isPipelineRow(app: App): boolean {
  return app.agentType === 'pipeline';
}

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Latest.jsx` — the default
 * public-project tab, backed by `usePublicApplicationsListQuery({...,
 * agents_type: 'pipeline'})` (baseline) / `useListPublicApplications` (this
 * app, `GET /elitea_core/public_applications/prompt_lib`).
 *
 * **Real, disclosed backend gap, WORSE here than the sibling `pages/agents/
 * Latest.tsx` (Wave-2 unit A1g) it mirrors:** `ListPublicApplicationsParams`
 * (`shared/api/generated/model/listPublicApplicationsParams.zod.ts`) has
 * exactly one field, `category` — the handler
 * (`internal/api/v2/eliteacore/handler.go:1251-1307`) hard-codes `LIMIT 50`
 * and never reads `agents_type`/`query`/`tags`/`sort_by`/`sort_order`/
 * pagination at all, so there is NO server-side way to ask for pipelines
 * only. Unlike the agents sibling (which has no field to filter on and
 * therefore does not filter), this page CAN and DOES filter client-side:
 * `PublicApplicationSummary` (the row schema this endpoint actually returns,
 * `shared/api/generated/model/publicApplicationSummary.zod.ts`, NOTE(W2):
 * `eliteacore/handler.go:1303-1312`) carries a real `agent_type` field per
 * row (`av.agent_type` in the SQL SELECT), surfaced by `entities/app`'s
 * `App.agentType`. This fetches the one available (unfiltered, up-to-50-row)
 * page, keeps only rows whose `agentType === 'pipeline'`, then applies the
 * search box over that filtered set via `entities/app`'s already-ported
 * `filterAppsByQuery`; "Load more" reveals more of that SAME already-fetched
 * array rather than issuing a second, impossible-to-parameterise request
 * (same disclosed pattern as `PrivatePipelinesList.tsx`, this unit). The
 * baseline's tag-category right rail (`Categories`/`TrendingAuthors`) is
 * dropped — no confirmed port, out of this unit's ownership fence.
 */
export function Latest(): ReactNode {
  const navigate = useNavigate();
  // The search box is the page HEADER's (`widgets/page-header`'s
  // `ListSearchField`); its value reaches this tab as the route's `query`
  // search param.
  const query = useListSearchQuery();
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);

  useEffect(() => {
    setVisibleCount(PAGE_SIZE);
  }, [query]);

  const listQuery = useListPublicApplications({});
  // `.data.data`'s declared type includes no error-envelope variant on this
  // endpoint (`listPublicApplicationsResponse` is 200-only) — `eliteaFetch`
  // throws for every non-2xx instead (mutator.ts's §3.6 unwrap contract).
  const wire = listQuery.data?.data;

  const filtered = useMemo(() => {
    const rows = wire ? normaliseAppPage(wire).rows.filter(isPipelineRow) : [];
    return filterAppsByQuery(rows, query);
  }, [wire, query]);

  const visibleRows = filtered.slice(0, visibleCount);
  const hasMore = visibleCount < filtered.length;

  return (
    <Box sx={containerSx}>
      <PipelineListPanel
        rows={visibleRows.map(
          (app): PipelineListRow => ({ id: app.id, name: app.name, description: app.description }),
        )}
        isLoading={listQuery.isFetching && wire === undefined}
        isError={listQuery.isError}
        errorMessage={t('pages.pipelines.latest.error', 'Failed to load pipelines.')}
        emptyTitle={
          query
            ? t('pages.pipelines.latest.emptyFound.title', 'No pipelines found.')
            : t('pages.pipelines.latest.empty.title', 'No public pipelines yet.')
        }
        emptyDescription={
          query
            ? t('pages.pipelines.latest.emptyFound.description', 'Create yours now!')
            : t('pages.pipelines.latest.empty.description', 'Publish yours now!')
        }
        onSelect={(id) => {
          void navigate({ to: '/pipelines/$tab/$agentId', params: { tab: 'latest', agentId: id } });
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
