/**
 * The Marketplace's reads: the agent and skill listings as one card list.
 *
 * Search and sort go to the SERVER's own parameters (`?query=`, `?sort_by=`,
 * `?sort_order=`) — the page keeps no client-side copy of the filter, so a
 * match past the first page is found. Categories are the one client-side
 * facet: the agent route files a row under one category and the skills route
 * under tags, and both are multi-select, which neither route's single-value
 * `category` parameter can express. So EVERY page of the answer is fetched
 * (the agent route's cap is 1000 rows a page, the skills route's 100) and the
 * whole set is narrowed in memory: a category never hides a match that sat on
 * a later page.
 *
 * Every request goes through `eliteaFetch`, which adds the `/api/v2` base and
 * returns orval's `{data,...}` envelope, so the body is read from `.data`.
 */
import { useMemo } from 'react';

import { useQuery } from '@tanstack/react-query';

import { useGetAgentCategories } from '@/shared/api/generated/applications/applications';
import type { PublicApplicationList } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { getConfig } from '@/shared/config';
import { useAllPublicSkills, useSkillCategories, type PublicSkillSummary } from '@/features/skills';

import { AGENT_LISTING_LIMIT, SORT_OPTIONS, type MarketplaceSortOption } from './constants';
import { agentCard, skillCard } from './listing';
import type { ApplicationData, ListingCardData, MarketplaceKind } from './types';

/** A runaway guard: 10 pages is 10000 agents, far past any catalogue. */
const AGENT_PAGE_LIMIT = 10;

/**
 * Public project id — per-deployment `VITE_PUBLIC_PROJECT_ID` runtime config.
 * `App.tsx` renders `MissingEnvPage` instead of mounting any route when the
 * config is missing, so the `'1'` fallback is unreachable in practice and
 * exists only so this stays a total function.
 */
function resolvePublicProjectId(): string {
  const config = getConfig();
  return config.status === 'ok' ? config.config.vite_public_project_id : '1';
}

export function sortOptionFor(key: string, kind: MarketplaceKind): MarketplaceSortOption {
  const option = SORT_OPTIONS.find((candidate) => candidate.key === key);
  const fallback = SORT_OPTIONS[0] as MarketplaceSortOption;
  if (option === undefined || (kind === 'skills' && option.agentsOnly)) return fallback;
  return option;
}

interface AgentRows {
  readonly rows: readonly ApplicationData[];
  readonly total: number;
}

async function fetchAgentPage(query: string, sort: MarketplaceSortOption, offset: number): Promise<PublicApplicationList> {
  const params = new URLSearchParams({
    limit: String(AGENT_LISTING_LIMIT),
    offset: String(offset),
    statuses: 'published',
    sort_by: sort.sortBy,
    sort_order: sort.sortOrder,
  });
  if (query !== '') params.set('query', query);
  const envelope = await eliteaFetch<{ data: PublicApplicationList }>(
    `/elitea_core/public_applications/prompt_lib?${params.toString()}`,
    { method: 'GET' },
  );
  return envelope.data;
}

async function fetchAgentRows(query: string, sort: MarketplaceSortOption): Promise<AgentRows> {
  const rows: ApplicationData[] = [];
  let total = 0;
  for (let page = 0; page < AGENT_PAGE_LIMIT; page += 1) {
    const result = await fetchAgentPage(query, sort, rows.length);
    total = result.total;
    rows.push(...result.rows);
    if (result.rows.length === 0 || rows.length >= total) break;
  }
  return { rows, total };
}

/** The agent rows for one search and sort. The listing page reads the same query, so opening a card reuses what the page already fetched. */
export function useAgentRows(query: string, sortKey: string, enabled = true) {
  const sort = sortOptionFor(sortKey, 'agents');
  return useQuery({
    queryKey: ['marketplace', 'agents', query, sort.key],
    queryFn: () => fetchAgentRows(query, sort),
    enabled,
  });
}

function sortSkills(cards: readonly ListingCardData[], sort: MarketplaceSortOption): ListingCardData[] {
  if (sort.key === 'name') return [...cards].sort((a, b) => a.name.localeCompare(b.name));
  return [...cards];
}

function skillCards(rows: readonly PublicSkillSummary[]): ListingCardData[] {
  return rows.map(skillCard).filter((card): card is ListingCardData => card !== null);
}

export interface MarketplaceListings {
  /** The listings in the chosen sort. */
  readonly cards: readonly ListingCardData[];
  /** The same listings, no search, newest first: what the Featured and Popular rows are cut from. */
  readonly newest: readonly ListingCardData[];
  /** The category names the Browse sidebar offers. */
  readonly categories: readonly string[];
  readonly isPending: boolean;
  readonly isError: boolean;
}

export interface MarketplaceRequest {
  readonly kind: MarketplaceKind;
  readonly query: string;
  readonly sortKey: string;
  /** The selected project: skill categories are read under it. */
  readonly projectId: string | undefined;
}

interface AgentCategoryBody {
  readonly categories?: readonly { readonly name: string }[];
}

/** The agent side of the page: the rows as cards, and the deployment's category names. */
function useAgentListings(query: string, sortKey: string, enabled: boolean): MarketplaceListings {
  const rows = useAgentRows(query, sortKey, enabled);
  const newest = useAgentRows('', 'newest', enabled);
  const categories = useGetAgentCategories(resolvePublicProjectId(), { query: { enabled } });
  const body = categories.data?.status === 200 ? (unwrapBody(categories.data) as AgentCategoryBody) : undefined;
  return useMemo(() => {
    const cards = (rows.data?.rows ?? []).map(agentCard);
    return {
      cards,
      newest: (newest.data?.rows ?? []).map(agentCard),
      categories: (body?.categories ?? []).map((category) => category.name).sort(),
      isPending: rows.isPending,
      isError: rows.isError,
    };
  }, [rows.data, newest.data, rows.isPending, rows.isError, body]);
}

/** The skill side of the page: the rows with a published version as cards, sorted here, and the skill categories. */
function useSkillListings(query: string, sortKey: string, projectId: string | undefined, enabled: boolean): MarketplaceListings {
  const sort = sortOptionFor(sortKey, 'skills');
  const rows = useAllPublicSkills(query, enabled);
  const unfiltered = useAllPublicSkills('', enabled);
  const categories = useSkillCategories(enabled ? projectId : undefined);
  return useMemo(() => {
    const server = skillCards(rows.data?.rows ?? []);
    return {
      cards: sortSkills(server, sort),
      // The route answers newest first; the shelves are cut from that order.
      newest: skillCards(unfiltered.data?.rows ?? []),
      categories: (categories.data ?? []).map((category) => category.name),
      isPending: rows.isPending,
      isError: rows.isError,
    };
  }, [rows.data, unfiltered.data, rows.isPending, rows.isError, categories.data, sort]);
}

export function useMarketplaceListings({ kind, query, sortKey, projectId }: MarketplaceRequest): MarketplaceListings {
  const trimmed = query.trim();
  const agents = useAgentListings(trimmed, sortKey, kind === 'agents');
  const skills = useSkillListings(trimmed, sortKey, projectId, kind === 'skills');
  return kind === 'agents' ? agents : skills;
}
