/**
 * The Marketplace (ADR-0051 decisions 1 and 8): shared agents and skills as
 * listings. This is the page `/marketplace` mounts; `/elitea-catalog` and
 * `/agents-hub` redirect here.
 *
 * The page is the grid, read through the facet search API. Search, kind, the
 * facets and sort live in the right sidebar's Browse tab
 * (`ui/MarketplaceBrowse.tsx`) and in the URL, and a summary bar above the grid
 * carries the active filters when that sidebar is closed. The Featured (the
 * editorial flag) and Popular rows show only when no search or facet is active
 * — a filtered list is the answer, not a shelf. The grid pages from the server
 * with Show more.
 */
import { useCallback, useEffect, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

import { SEARCH_DEBOUNCE_MS } from './constants';
import {
  filtersFromSearch,
  hasActiveFilter,
  searchFromFilters,
  type MarketplaceFilters,
  type MarketplaceSearch,
} from './filters';
import { kindLabel } from './listing';
import type { ListingCardData, MarketplaceKind } from './types';
import { ListingSection } from './ui/ListingSection';
import { MarketplaceBrowse } from './ui/MarketplaceBrowse';
import { MarketplaceSummaryBar } from './ui/MarketplaceSummaryBar';
import { useLegacyAgentLink } from './useLegacyAgentLink';
import { useMarketplaceSearch, useMarketplaceShelf, type MarketplaceListings } from './useMarketplaceListings';

/** An old agent link the redirect could not resolve: finished here once a project is known, with a retry when the lookup fails. */
function LegacyLinkNotice({
  projectId,
  agentId,
}: {
  readonly projectId: string | undefined;
  readonly agentId: string | undefined;
}): ReactNode {
  const link = useLegacyAgentLink(projectId, agentId);
  if (!link.failed) return null;
  return (
    <Box sx={{ display: 'flex', gap: 1, alignItems: 'center' }} data-testid="marketplace-legacy-link-error">
      <BannerMessage
        variant="error"
        message={t('marketplace.legacyLink.error', 'The link to that agent did not open.')}
      />
      <Button size="small" onClick={link.retry} data-testid="marketplace-legacy-link-retry">
        {t('marketplace.legacyLink.retry', 'Retry')}
      </Button>
    </Box>
  );
}

export function Marketplace(): ReactNode {
  // Read loosely, per this codebase's convention: a page must not depend on which route file mounts it.
  const search = useSearch({ strict: false }) as MarketplaceSearch;
  const navigate = useNavigate();
  const projectId = useSelectedProjectId();
  const filters = filtersFromSearch(search);
  const { kind } = filters;

  // What the box shows; the URL (and so the server) trails it by `SEARCH_DEBOUNCE_MS`.
  const [typedQuery, setTypedQuery] = useState(filters.query);
  useEffect(() => setTypedQuery(filters.query), [filters.query]);

  const apply = useCallback(
    (next: MarketplaceFilters): void => {
      void navigate({ to: '/marketplace', search: searchFromFilters(next), replace: true });
    },
    [navigate],
  );
  const change = (patch: Partial<MarketplaceFilters>): void => {
    if (patch.query !== undefined) setTypedQuery(patch.query);
    apply({ ...filters, ...patch });
  };
  useEffect(() => {
    if (typedQuery === filters.query) return undefined;
    const timer = setTimeout(() => apply({ ...filters, query: typedQuery }), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [typedQuery, filters, apply]);

  const listings = useMarketplaceSearch(projectId, filters);
  const filtered = hasActiveFilter(filters);
  const featured = useMarketplaceShelf(projectId, kind, 'featured', !filtered);
  const popular = useMarketplaceShelf(projectId, kind, 'popular', !filtered);

  const changeKind = (next: MarketplaceKind): void => {
    if (next !== kind) apply({ ...filters, kind: next, categories: [], tags: [], requires: [] });
  };
  const clearAll = (): void => {
    setTypedQuery('');
    apply({
      ...filters,
      query: '',
      categories: [],
      tags: [],
      publisherId: '',
      maxTier: '',
      requires: [],
      minRating: 0,
      hiredHere: false,
    });
  };

  const label = kindLabel(kind);
  const summary = t('marketplace.summary.count', '{{kind}}: {{count}}', { count: listings.total, kind: label });

  return (
    <Box data-testid="marketplace" sx={pageSx}>
      <MarketplaceBrowse
        filters={filters}
        typedQuery={typedQuery}
        onTypedQueryChange={setTypedQuery}
        facets={listings.facets}
        onChange={change}
        onKindChange={changeKind}
      />
      <Box sx={bodySx}>
        <MarketplaceSummaryBar
          summary={summary}
          filters={{ ...filters, query: typedQuery }}
          onChange={change}
          onClearAll={clearAll}
        />
        <LegacyLinkNotice projectId={projectId} agentId={search.agentId} />
        <ListingResults
          listings={listings}
          featured={featured}
          popular={popular}
          kindLabel={label}
          filtered={filtered}
        />
      </Box>
    </Box>
  );
}

interface ListingResultsProps {
  readonly listings: MarketplaceListings;
  readonly featured: readonly ListingCardData[];
  readonly popular: readonly ListingCardData[];
  readonly kindLabel: string;
  /** A search or a facet is active. */
  readonly filtered: boolean;
}

/** What is under the summary bar: the loading, error and empty states, the two shelves and the full list. */
function ListingResults({ listings, featured, popular, kindLabel, filtered }: ListingResultsProps): ReactNode {
  if (listings.isPending) return <LinearProgress />;
  if (listings.isError) {
    return (
      <BannerMessage
        variant="error"
        message={t('marketplace.loadError', 'The listings did not load. Reload the page to try again.')}
      />
    );
  }
  if (listings.cards.length === 0 && !filtered) {
    return (
      <NoResultsMessage
        title={t('marketplace.empty.title', 'Nothing is published yet')}
        description={t(
          'marketplace.empty.description',
          'Publish an agent or a skill from its page in the Workshop and it appears here.',
        )}
      />
    );
  }
  if (listings.cards.length === 0) {
    return (
      <NoResultsMessage
        title={t('marketplace.noResults.title', 'No listings match')}
        description={t('marketplace.noResults.description', 'Clear a filter or try different search terms.')}
      />
    );
  }
  return (
    <>
      <ListingSection id="featured" title={t('marketplace.featured', 'Featured')} cards={filtered ? [] : featured} />
      <ListingSection
        id="popular"
        title={t('marketplace.popular', 'Popular in your organisation')}
        cards={filtered ? [] : popular}
      />
      <ListingSection
        id="all"
        title={t('marketplace.all', 'All {{kind}}', { kind: kindLabel.toLowerCase() })}
        cards={listings.cards}
      />
      {listings.hasNextPage && (
        <Button
          variant="outlined"
          sx={{ alignSelf: 'center' }}
          disabled={listings.isFetchingNextPage}
          onClick={listings.fetchNextPage}
          data-testid="marketplace-show-more"
        >
          {t('marketplace.showMore', 'Show more')}
        </Button>
      )}
    </>
  );
}

const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column', minHeight: 0 };

const bodySx: SxProps<Theme> = {
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
  display: 'flex',
  flexDirection: 'column',
  gap: '1.5rem',
  padding: '0 1.5rem 1.5rem',
};
