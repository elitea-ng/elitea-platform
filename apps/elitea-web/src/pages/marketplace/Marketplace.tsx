/**
 * The Marketplace (ADR-0051 decisions 1 and 8): shared agents and skills as
 * listings. This is the page `/marketplace` mounts; `/elitea-catalog` and
 * `/agents-hub` redirect here.
 *
 * The page is the grid. Search, kind, categories and sort live in the right
 * sidebar's Browse tab (`ui/MarketplaceBrowse.tsx`), and a summary bar above
 * the grid carries the active filters when that sidebar is closed. The
 * Featured and Popular rows show only when no search or category is active —
 * a filtered list is the answer, not a shelf. There is no per-category
 * "Show more": one list, narrowed by the facets.
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

import { SEARCH_DEBOUNCE_MS } from './constants';
import { featuredRow, filterByCategories, kindFromSearch, popularRow } from './listing';
import type { ListingCardData, MarketplaceKind } from './types';
import { ListingSection } from './ui/ListingSection';
import { MarketplaceBrowse } from './ui/MarketplaceBrowse';
import { MarketplaceSummaryBar } from './ui/MarketplaceSummaryBar';
import { sortOptionFor, useMarketplaceListings, type MarketplaceListings } from './useMarketplaceListings';

interface MarketplaceSearch {
  kind?: string;
}

/** A stable empty pick, so the memos below do not see a new array every render. */
const NO_CATEGORIES: readonly string[] = [];

interface PickedCategories {
  readonly kind: MarketplaceKind;
  readonly names: readonly string[];
}

export function Marketplace(): ReactNode {
  // Read loosely, per this codebase's convention: a page must not depend on which route file mounts it.
  const search = useSearch({ strict: false }) as MarketplaceSearch;
  const navigate = useNavigate();
  const projectId = useSelectedProjectId();
  const kind = kindFromSearch(search.kind);

  const [query, setQuery] = useState('');
  // What the server is asked: it trails the box by `SEARCH_DEBOUNCE_MS`.
  const [submittedQuery, setSubmittedQuery] = useState('');
  const [sortKey, setSortKey] = useState('newest');
  // Remembered with its kind, so a switch to the other kind starts with no category (switching back restores the earlier pick).
  const [picked, setPicked] = useState<PickedCategories>({ kind, names: [] });
  const selectedCategories = picked.kind === kind ? picked.names : NO_CATEGORIES;

  useEffect(() => {
    const timer = setTimeout(() => setSubmittedQuery(query), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [query]);

  const listings = useMarketplaceListings({ kind, query: submittedQuery, sortKey, projectId });
  const shown = useMemo(() => filterByCategories(listings.cards, selectedCategories), [listings.cards, selectedCategories]);

  const noFilter = submittedQuery.trim() === '' && selectedCategories.length === 0;
  const featured = useMemo(() => (noFilter ? featuredRow(listings.newest) : []), [noFilter, listings.newest]);
  const popular = useMemo(() => (noFilter ? popularRow(listings.newest) : []), [noFilter, listings.newest]);

  const toggleCategory = (category: string): void => {
    setPicked({ kind, names: selectedCategories.includes(category) ? selectedCategories.filter((name) => name !== category) : [...selectedCategories, category] });
  };
  const clearCategories = (): void => setPicked({ kind, names: [] });
  const changeKind = (next: MarketplaceKind): void => {
    if (next === kind) return;
    // The sort key may be the other kind's alone ("Most liked"); the option resolver falls back, and the box shows the same.
    setSortKey((current) => sortOptionFor(current, next).key);
    void navigate({ to: '/marketplace', search: { kind: next }, replace: true });
  };

  const kindLabel = kind === 'agents' ? t('marketplace.kind.agents', 'Agents') : t('marketplace.kind.skills', 'Skills');
  const summary = t('marketplace.summary.count', '{{kind}}: {{count}}', { count: shown.length, kind: kindLabel });

  return (
    <Box data-testid="marketplace" sx={pageSx}>
      <MarketplaceBrowse
        kind={kind}
        onKindChange={changeKind}
        query={query}
        onQueryChange={setQuery}
        categories={listings.categories}
        selectedCategories={selectedCategories}
        onToggleCategory={toggleCategory}
        onClearCategories={clearCategories}
        sortKey={sortOptionFor(sortKey, kind).key}
        onSortChange={setSortKey}
      />
      <Box sx={bodySx}>
        <MarketplaceSummaryBar
          summary={summary}
          query={query}
          categories={selectedCategories}
          onClearQuery={() => setQuery('')}
          onRemoveCategory={toggleCategory}
          onClearAll={() => {
            setQuery('');
            clearCategories();
          }}
        />
        <ListingResults listings={listings} shown={shown} featured={featured} popular={popular} kindLabel={kindLabel} />
      </Box>
    </Box>
  );
}

interface ListingResultsProps {
  readonly listings: MarketplaceListings;
  readonly shown: readonly ListingCardData[];
  readonly featured: readonly ListingCardData[];
  readonly popular: readonly ListingCardData[];
  readonly kindLabel: string;
}

/** What is under the summary bar: the loading, error and empty states, the two shelves and the full list. */
function ListingResults({ listings, shown, featured, popular, kindLabel }: ListingResultsProps): ReactNode {
  if (listings.isPending) return <LinearProgress />;
  if (listings.isError) {
    return <BannerMessage variant="error" message={t('marketplace.loadError', 'The listings did not load. Reload the page to try again.')} />;
  }
  if (shown.length === 0) {
    return (
      <NoResultsMessage
        title={t('marketplace.noResults.title', 'No listings match')}
        description={t('marketplace.noResults.description', 'Clear a filter or try different search terms.')}
      />
    );
  }
  return (
    <>
      <ListingSection id="featured" title={t('marketplace.featured', 'Featured')} cards={featured} />
      <ListingSection id="popular" title={t('marketplace.popular', 'Popular in your organisation')} cards={popular} />
      <ListingSection id="all" title={t('marketplace.all', 'All {{kind}}', { kind: kindLabel.toLowerCase() })} cards={shown} />
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

