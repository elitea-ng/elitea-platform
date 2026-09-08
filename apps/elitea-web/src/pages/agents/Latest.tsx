import { useEffect, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate } from '@tanstack/react-router';

import { filterAppsByQuery, normaliseAppPage } from '@/entities/app';
import { useListPublicApplications } from '@/shared/api/generated/applications/applications';
import { t } from '@/shared/i18n';
import { useListSearchQuery } from '@/widgets/page-header';

import { ApplicationListPanel, type ApplicationListRow } from './ui/ApplicationListPanel';

const PAGE_SIZE = 20;

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Latest.jsx` — the
 * default public-project tab, backed by `usePublicApplicationsListQuery`
 * (baseline) / `useListPublicApplications` (this app,
 * `GET /elitea_core/public_applications/prompt_lib`).
 *
 * **Real, disclosed backend gap:** `ListPublicApplicationsParams`
 * (`shared/api/generated/model/listPublicApplicationsParams.zod.ts`) has
 * exactly one field, `category` — the handler
 * (`internal/api/v2/eliteacore/handler.go:1251-1317`) hard-codes `LIMIT 50`
 * and never reads `query`/`tags`/`sort_by`/`sort_order`/pagination at all.
 * This fetches the one available (unfiltered, up-to-50-row) page and applies
 * the search box client-side via `entities/app`'s already-ported
 * `filterAppsByQuery`; "Load more" reveals more of that SAME already-fetched
 * array rather than issuing a second, impossible-to-parameterise request
 * (same disclosed pattern as `PrivateAgentsList.tsx`, this unit). The
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
    const rows = wire ? normaliseAppPage(wire).rows : [];
    return filterAppsByQuery(rows, query);
  }, [wire, query]);

  const visibleRows = filtered.slice(0, visibleCount);
  const hasMore = visibleCount < filtered.length;

  return (
    <Box sx={containerSx}>
      <ApplicationListPanel
        rows={visibleRows.map(
          (app): ApplicationListRow => ({ id: app.id, name: app.name, description: app.description }),
        )}
        isLoading={listQuery.isFetching && wire === undefined}
        isError={listQuery.isError}
        errorMessage={t('pages.agents.latest.error', 'Failed to load agents.')}
        emptyTitle={
          query
            ? t('pages.agents.latest.emptyFound.title', 'No agents found.')
            : t('pages.agents.latest.empty.title', 'No public agents yet.')
        }
        emptyDescription={
          query
            ? t('pages.agents.latest.emptyFound.description', 'Create yours now!')
            : t('pages.agents.latest.empty.description', 'Publish yours now!')
        }
        onSelect={(id) => {
          void navigate({ to: '/agents/$tab/$agentId', params: { tab: 'latest', agentId: id } });
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
