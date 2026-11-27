/**
 * What a listing is, and how a search row becomes a card.
 *
 * The Marketplace shows shared agents and skills the same way: a card that is
 * a link to `/marketplace/{kind}/{listingId}`. The search route answers one row
 * shape for both kinds, so a row is mapped here once and nothing past this file
 * cares which kind it holds.
 */
import type { MarketplaceListingSummary, PublicApplicationSummary } from '@/shared/api/generated/model';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';

import type { ListingCardData, MarketplaceKind } from './types';

/** Which kind a raw `?kind=` value selects; anything but the literal `skills` or `teams` is agents, so a stale or hand-typed value degrades to the default. */
export function kindFromSearch(kind: string | undefined): MarketplaceKind {
  if (kind === 'skills' || kind === 'teams') return kind;
  return 'agents';
}

const API_KINDS: Readonly<Record<MarketplaceKind, 'agent' | 'skill' | 'team'>> = {
  agents: 'agent',
  skills: 'skill',
  teams: 'team',
};

/** The route's kind word for a page kind. */
export function apiKind(kind: MarketplaceKind): 'agent' | 'skill' | 'team' {
  return API_KINDS[kind];
}

/** The page kind of the route's kind word; an unknown word reads as agents. */
function pageKind(kind: string): MarketplaceKind {
  if (kind === 'skill') return 'skills';
  if (kind === 'team') return 'teams';
  return 'agents';
}

/** The kind's plural label: Agents, Skills or Teams. */
export function kindLabel(kind: MarketplaceKind): string {
  if (kind === 'skills') return t('marketplace.kind.skills', 'Skills');
  if (kind === 'teams') return t('marketplace.kind.teams', 'Teams');
  return t('marketplace.kind.agents', 'Agents');
}

/** A search row as a card. */
export function listingCard(row: MarketplaceListingSummary): ListingCardData {
  return {
    kind: pageKind(row.kind),
    id: row.id,
    entityId: row.entity_id,
    name: row.name,
    description: row.description,
    iconUrl: row.icon_url ?? undefined,
    publisher: row.publisher?.name,
    ratingAvg: row.rating_avg ?? null,
    ratingCount: row.rating_count,
    hires: row.hires,
    likes: row.likes,
    isLiked: row.is_liked,
    version: row.current_version?.name,
    versionId: row.current_version?.id,
    tier: row.tier,
    requiredTypes: row.required_types,
    hiredVersion: row.hired[0]?.version_name,
    categories: row.category === '' ? [] : [row.category],
    delisted: row.state === 'delisted',
  };
}

/** The listing page's URL parts for a card. */
export function listingParams(card: ListingCardData): { readonly kind: MarketplaceKind; readonly listingId: string } {
  return { kind: card.kind, listingId: card.id };
}

/**
 * The public project id: per-deployment `VITE_PUBLIC_PROJECT_ID` runtime
 * config. `App.tsx` renders `MissingEnvPage` instead of mounting any route when
 * the config is missing, so the `'1'` fallback is unreachable in practice and
 * exists only so this stays a total function.
 */
export function resolvePublicProjectId(): string {
  const config = getConfig();
  return config.status === 'ok' ? config.config.vite_public_project_id : '1';
}

/** The catalogue row Try in chat and the like button read, rebuilt from a listing: the public copy at its current version. */
export function publicApplicationOf(row: MarketplaceListingSummary): PublicApplicationSummary {
  return {
    project_id: resolvePublicProjectId(),
    id: row.entity_id,
    name: row.name,
    description: row.description,
    version_id: row.current_version?.id ?? '',
    version_name: row.current_version?.name ?? '',
    agent_type: 'agent',
    meta: null,
    tags: [],
    likes: row.likes,
    is_liked: row.is_liked,
  };
}

/** The rating as text, never as a colour or an icon alone (ADR-0051 accessibility). */
export function ratingText(card: { readonly ratingAvg: number | null; readonly ratingCount: number }): string {
  if (card.ratingAvg === null || card.ratingCount === 0) return t('marketplace.card.noRatings', 'No ratings yet');
  return t('marketplace.card.rating', 'Rated {{rating}} out of 5 by {{count}}', {
    rating: card.ratingAvg.toFixed(1),
    count: card.ratingCount,
  });
}
