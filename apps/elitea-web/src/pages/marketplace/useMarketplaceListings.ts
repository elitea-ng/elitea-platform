/**
 * The Marketplace's reads: the agent and skill listings as one card list.
 *
 * Search and sort go to the SERVER's own parameters (`?query=`, `?sort_by=`,
 * `?sort_order=`) — the page keeps no client-side copy of the filter, so a
 * match past the first page is found. Categories are the one client-side
 * facet: the agent route files a row under one category and the skills route
 * under tags, and both are multi-select, which neither route's single-value
 * `category` parameter can express. The whole page (up to the route's cap) is
 * fetched once and narrowed in memory.
 *
 * Every request goes through `eliteaFetch`, which adds the `/api/v2` base and
 * returns orval's `{data,...}` envelope, so the body is read from `.data`.
 */
import { useMemo } from 'react';

import { useQuery } from '@tanstack/react-query';

import { useGetAgentCategories } from '@/shared/api/generated/applications/applications';
import type { PublicApplicationList } from '@/shared/api/generated/model';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { getConfig } from '@/shared/config';
import { usePublicSkills, useSkillCategories, type PublicSkillSummary } from '@/features/skills';

import { AGENT_LISTING_LIMIT, SKILL_LISTING_LIMIT, SORT_OPTIONS, type MarketplaceSortOption } from './constants';
import { agentCard, skillCard } from './listing';
import type { ApplicationData, ListingCardData, MarketplaceKind } from './types';

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

async function fetchAgentRows(query: string, sort: MarketplaceSortOption): Promise<AgentRows> {
  const params = new URLSearchParams({
    limit: String(AGENT_LISTING_LIMIT),
    offset: '0',
    statuses: 'published',
    sort_by: sort.sortBy,
    sort_order: sort.sortOrder,
  });
  if (query !== '') params.set('query', query);
  const envelope = await eliteaFetch<{ data: PublicApplicationList }>(
    `/elitea_core/public_applications/prompt_lib?${params.toString()}`,
    { method: 'GET' },
  );
  return { rows: envelope.data.rows, total: envelope.data.total };
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
  return [...cards].sort((a, b) => b.recency - a.recency);
}

function skillCards(rows: readonly PublicSkillSummary[], sort: MarketplaceSortOption): ListingCardData[] {
  const cards = rows.map(skillCard).filter((card): card is ListingCardData => card !== null);
  return sortSkills(cards, sort);
}

export interface MarketplaceListings {
  readonly cards: readonly ListingCardData[];
  /** Listings the server says match the search; more than `cards` when the page cap cut the answer short. */
  readonly total: number;
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

/** The agent side of the page: the rows as cards, and the deployment's category names. */
function useAgentListings(query: string, sortKey: string, enabled: boolean): MarketplaceListings {
  const rows = useAgentRows(query, sortKey, enabled);
  const categories = useGetAgentCategories(resolvePublicProjectId(), { query: { enabled } });
  const body = categories.data?.status === 200 ? categories.data.data : undefined;
  return useMemo(() => {
    const names = body?.categories ?? [];
    const cards = (rows.data?.rows ?? []).map(agentCard);
    return {
      cards,
      total: rows.data?.total ?? cards.length,
      categories: names.map((category) => category.name).sort(),
      isPending: rows.isPending,
      isError: rows.isError,
    };
  }, [rows.data, rows.isPending, rows.isError, body]);
}

/** The skill side of the page: the rows with a published version as cards, sorted here, and the skill categories. */
function useSkillListings(query: string, sortKey: string, projectId: string | undefined, enabled: boolean): MarketplaceListings {
  const sort = sortOptionFor(sortKey, 'skills');
  const rows = usePublicSkills({ ...(query === '' ? {} : { query }), limit: SKILL_LISTING_LIMIT }, enabled);
  const categories = useSkillCategories(enabled ? projectId : undefined);
  return useMemo(() => {
    const cards = skillCards(rows.data?.rows ?? [], sort);
    return {
      cards,
      total: rows.data?.total ?? cards.length,
      categories: (categories.data ?? []).map((category) => category.name),
      isPending: rows.isPending,
      isError: rows.isError,
    };
  }, [rows.data, rows.isPending, rows.isError, categories.data, sort]);
}

export function useMarketplaceListings({ kind, query, sortKey, projectId }: MarketplaceRequest): MarketplaceListings {
  const trimmed = query.trim();
  const agents = useAgentListings(trimmed, sortKey, kind === 'agents');
  const skills = useSkillListings(trimmed, sortKey, projectId, kind === 'skills');
  return kind === 'agents' ? agents : skills;
}
