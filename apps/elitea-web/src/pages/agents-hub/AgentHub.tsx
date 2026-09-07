/**
 * Agents Hub page — main landing for discovering agents.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/agent-hub/AgentHub.jsx`.
 *
 * Deviations:
 *  - No tour integration (A13 §6.6: tour targets dropped for agents-hub).
 *  - No Redux — uses zustand-based `useAgentHubData`.
 *  - No AgentHubContext — state updates go directly through hooks.
 *  - Route wiring is out of scope.
 *
 * ── Adversarial-review fixes (cluster A13-agents-hub) ────────────────────
 *  - Finding 8: the `?agentId=` deep link (the `/agents-hub` route already
 *    declares/validates it — `routes/_shell/agents-hub.tsx`'s
 *    `validateSearch: pickParams('agentId')`) is read again and auto-opens
 *    that agent's modal once it shows up in the already-fetched hub data.
 *    Unlike the baseline (which opens the modal with a bare `{id: agentId}`
 *    stub and lets `AgentModal` hydrate it), this looks the id up in
 *    `applicationsByTag` — which already carries the full `ApplicationData`
 *    (including `version_name`, which `AgentModal`'s version-detail fetch
 *    needs, per finding 2's fix) — so no extra undocumented endpoint call
 *    is needed. If the id never shows up (e.g. it fell outside the
 *    backend's hardcoded 50-row cap — see `useAgentHubData.ts`'s own doc
 *    comment on findings 5/6), the modal simply never opens; there is no
 *    other agent-by-id lookup in this API surface to fall back to.
 *  - Issue #36 item 8: the search box and the sort control now drive the
 *    SERVER's own parameters (`?query=`, `?sort_by=`, `?sort_order=`) rather
 *    than a client-side filter over whatever page happened to be in memory.
 *    The catalogue handler reads them, counts `total` over the filtered set,
 *    and pages with `limit`/`offset`, so a match on page two is found and
 *    "Show more" can reach it. A client-side name filter over a truncated
 *    page silently answered "No agents found" for anything past the cap.
 *  - Finding 9: the search box + category-filter chips (dropped entirely
 *    in the port) are restored using `shared/ui/CategoryFilter` — the same
 *    chrome component `pages/credentials/CredentialsTypesPanel.tsx` and
 *    `features/toolkits/ui/ToolkitTypeSelector.tsx` already use. This
 *    turns `selectedTagNames`/`useAgentHubData`'s `filteredByTag` (real
 *    logic that was simply never triggered — its setter was dropped from
 *    the `useState` destructure) into a live code path.
 *    NOT using `shared/ui/GroupedCategory` alongside it (the pattern
 *    `pages/credentials/CredentialTypeSelector.tsx` establishes): that
 *    component's `renderCategory` is typed against `shared/ui/
 *    CategoryItemCard`'s generic `CategoryItem` (`{key, label, icon,
 *    onClick}`), which has no slot for the full `ApplicationData`
 *    `AgentCard`/`AgentHubLike` need (icon, authors, live like state) —
 *    converting to that shape would drop exactly the data findings 1/4
 *    already fixed. `AgentCategorySection` already does its own
 *    "given a category + `ApplicationData[]`, render cards + show more"
 *    grouping, so only `CategoryFilter`'s chrome (title/search/chips
 *    wrapping arbitrary `children`) is reused here, not the whole
 *    `GroupedCategory` contract.
 */
import { memo, useCallback, useEffect, useMemo, useState, type ChangeEvent } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';

import { useSearch } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { CategoryFilter } from '@/shared/ui/CategoryFilter';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

import { useAgentHubData } from './useAgentHubData';
import { AgentCategorySection, AgentModal } from './ui';
import {
  MY_LIKED_CATEGORY,
  OTHER_CATEGORY,
  SEARCH_DEBOUNCE_MS,
  SORT_OPTIONS,
  TRENDING_CATEGORY,
} from './constants';
import type { ApplicationData } from './types';

export interface AgentHubProps {
  // Intentionally empty — route props are handled at the route layer.
}

/**
 * `/agents-hub`'s `validateSearch` (`routes/_shell/agents-hub.tsx`)
 * declares only `agentId` — read loosely per this codebase's established
 * `useSearch({ strict: false }) as <Search>` convention (see e.g.
 * `pages/agents/EditApplication.tsx`) rather than `{ from: routeId }`,
 * since a page must not depend on which route file mounts it.
 */
interface AgentHubSearch {
  agentId?: string;
}

const AgentHub = memo(() => {
  const search = useSearch({ strict: false }) as AgentHubSearch;
  const [selectedTagNames, setSelectedTagNames] = useState<string[]>([]);
  const [query, setQuery] = useState('');
  /**
   * The text actually sent to the server. `query` is what the box shows; this
   * trails it by `SEARCH_DEBOUNCE_MS` so a typed word is one request, not one
   * request per keystroke.
   */
  const [submittedQuery, setSubmittedQuery] = useState('');
  const [sortKey, setSortKey] = useState<string>(SORT_OPTIONS[0].key);
  const [selectedApplication, setSelectedApplication] = useState<ApplicationData | null>(null);
  const [isModalOpen, setIsModalOpen] = useState(false);
  const [hasOpenedDeepLink, setHasOpenedDeepLink] = useState(false);

  useEffect(() => {
    const timer = setTimeout(() => { setSubmittedQuery(query); }, SEARCH_DEBOUNCE_MS);
    return () => { clearTimeout(timer); };
  }, [query]);

  const sortOption = useMemo(
    () => SORT_OPTIONS.find(option => option.key === sortKey) ?? SORT_OPTIONS[0],
    [sortKey],
  );

  const {
    allCategories,
    applicationsByTag,
    totalCountsByTag,
    loadingTags,
    refreshingTags,
    error,
    hasMore,
    loadMore,
    onRefresh,
  } = useAgentHubData(selectedTagNames, {
    query: submittedQuery,
    sortBy: sortOption.sortBy,
    sortOrder: sortOption.sortOrder,
  });

  const handleApplicationSelect = useCallback((app: ApplicationData) => {
    setSelectedApplication(app);
    setIsModalOpen(true);
  }, []);

  const handleCloseModal = useCallback(() => {
    setIsModalOpen(false);
    setSelectedApplication(null);
  }, []);

  /**
   * "Show more" on an ordinary category asks the SERVER for the next page.
   * Trending and My Liked are their own single-page queries, so there is
   * nothing further to ask for there.
   */
  const handleLoadMore = useCallback(
    (category: string) => {
      if (category === TRENDING_CATEGORY || category === MY_LIKED_CATEGORY) return;
      if (!hasMore) return;
      void loadMore();
    },
    [hasMore, loadMore],
  );

  const handleSelectCategory = useCallback((category: string) => {
    setSelectedTagNames(prev =>
      prev.includes(category) ? prev.filter(name => name !== category) : [...prev, category],
    );
  }, []);

  const handleSearchChange = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    setQuery(event.target.value);
  }, []);

  const handleSortChange = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    setSortKey(event.target.value);
  }, []);

  // Finding 8: auto-open the deep-linked agent's modal once it shows up in
  // the already-fetched hub data (see this file's own module doc comment).
  useEffect(() => {
    if (!search.agentId || hasOpenedDeepLink) return;
    const match = Object.values(applicationsByTag)
      .flat()
      .find(app => app.id === search.agentId);
    if (!match) return;
    setSelectedApplication(match);
    setIsModalOpen(true);
    setHasOpenedDeepLink(true);
  }, [search.agentId, hasOpenedDeepLink, applicationsByTag]);

  const visibleCategories = useMemo(
    () => allCategories.filter(cat => cat !== OTHER_CATEGORY),
    [allCategories],
  );

  /**
   * The rows come back already filtered by the server's `?query=`, so nothing
   * is filtered again here. A second client-side pass over the page in hand
   * is what made the search look broken for anything outside it.
   */
  const visibleItemsByCategory = useMemo(() => {
    const result: Record<string, ApplicationData[]> = {};
    visibleCategories.forEach(category => {
      result[category] = applicationsByTag[category] || [];
    });
    return result;
  }, [visibleCategories, applicationsByTag]);

  const hasAnyVisibleItems = useMemo(
    () => Object.values(visibleItemsByCategory).some(items => items.length > 0),
    [visibleItemsByCategory],
  );

  /*
   * DEFECT this fixes: `useAgentHubData` reports a refused list through
   * `error`. No component reads that value. A failed load shows the ordinary
   * "No agents found" message. The user cannot tell a broken hub from an
   * empty catalogue.
   *
   * The banner replaces the empty state on a failure. The banner stays above
   * the list when some categories load. That list is incomplete.
   */
  const showNoResults = !hasAnyVisibleItems && error === null;

  const renderCategory = useCallback(
    (category: string, items: ApplicationData[]) => (
      <AgentCategorySection
        key={category}
        category={category}
        items={items}
        totalCount={totalCountsByTag[category] || 0}
        isLoading={refreshingTags.has(category)}
        isLoadingMore={loadingTags.has(category)}
        onSelectItem={handleApplicationSelect}
        onLoadMore={handleLoadMore}
        onRefresh={(cat: string) => { void onRefresh(cat); }}
      />
    ),
    [handleApplicationSelect, handleLoadMore, loadingTags, onRefresh, refreshingTags, totalCountsByTag],
  );

  const styles: Record<string, SxProps<Theme>> = {
    workspace: {
      display: 'flex',
      flexDirection: 'column',
      height: '100%',
    },
    sections: {
      display: 'flex',
      flexDirection: 'column',
      gap: '2rem',
      width: '100%',
    },
    sortRow: {
      display: 'flex',
      justifyContent: 'flex-end',
      width: '100%',
      paddingBottom: '1rem',
    },
    sortField: {
      minWidth: '12rem',
    },
  };

  return (
    <Box sx={styles.workspace}>
      <CategoryFilter
        title={t('agentsHub.title', 'Welcome to Agent HUB')}
        searchPlaceholder={t('agentsHub.searchPlaceholder', 'Search for agents')}
        searchQuery={query}
        onSearchChange={handleSearchChange}
        allCategories={visibleCategories}
        selectedCategories={selectedTagNames}
        onSelectCategory={handleSelectCategory}
      >
        <Box sx={styles.sortRow}>
          <TextField
            select
            size="small"
            value={sortKey}
            onChange={handleSortChange}
            label={t('agentsHub.sortBy', 'Sort by')}
            sx={styles.sortField}
            slotProps={{ htmlInput: { 'aria-label': t('agentsHub.sortBy', 'Sort by') } }}
          >
            {SORT_OPTIONS.map(option => (
              <MenuItem
                key={option.key}
                value={option.key}
              >
                {t(option.labelKey, option.labelFallback)}
              </MenuItem>
            ))}
          </TextField>
        </Box>
        {error !== null && (
          <BannerMessage
            variant="error"
            message={t(
              'agentsHub.loadError',
              'The agent list did not load. Reload the page to try again.',
            )}
          />
        )}
        {hasAnyVisibleItems && (
          <Box sx={styles.sections}>
            {visibleCategories.map(category => {
              const items = visibleItemsByCategory[category] || [];
              return items.length > 0 ? renderCategory(category, items) : null;
            })}
          </Box>
        )}
        {showNoResults && (
          <NoResultsMessage
            title={t('agentsHub.noResults.title', 'No agents found')}
            description={t('agentsHub.noResults.description', 'Try adjusting your search terms')}
          />
        )}
      </CategoryFilter>
      {selectedApplication && (
        <AgentModal
          open={isModalOpen}
          onClose={handleCloseModal}
          agent={selectedApplication}
        />
      )}
    </Box>
  );
});

AgentHub.displayName = 'AgentHub';

export default AgentHub;
