/**
 * What a listing is, and how the two kinds of catalogue row become one card.
 *
 * The Marketplace shows shared agents and skills the same way: a card that is
 * a link to `/marketplace/{kind}/{listingId}`. The rows come from two
 * different routes (`public_applications`, `public_skills`), so they are
 * mapped here once and nothing past this file cares which kind it holds.
 */
import type { PublicSkillSummary } from '@/features/skills';

import { FEATURED_COUNT, POPULAR_COUNT } from './constants';
import { getCategoryForApplication } from './helpers';
import type { ApplicationData, ListingCardData, MarketplaceKind } from './types';

/** Which kind a raw `?kind=` value selects; anything but the literal `skills` is agents, so a stale or hand-typed value degrades to the default. */
export function kindFromSearch(kind: string | undefined): MarketplaceKind {
  return kind === 'skills' ? 'skills' : 'agents';
}

function iconUrlOf(meta: ApplicationData['meta']): string | undefined {
  const url = meta?.icon_meta?.['url'];
  return typeof url === 'string' && url !== '' ? url : undefined;
}

export function agentCard(app: ApplicationData): ListingCardData {
  return {
    kind: 'agents',
    id: String(app.id),
    name: app.name,
    description: app.description,
    iconUrl: iconUrlOf(app.meta),
    likes: app.likes,
    version: app.version_name === '' ? undefined : app.version_name,
    categories: [getCategoryForApplication(app)],
    recency: Number(app.version_id) || 0,
  };
}

/** The published version a skill row offers: only a published one is attachable. */
function publishedVersionOf(skill: PublicSkillSummary): { readonly id: number; readonly name: string | undefined } | undefined {
  const published = skill.versions?.find((version) => version.status === 'published');
  return published?.id === undefined ? undefined : { id: published.id, name: published.name };
}

/** A skill row as a card, or `null` when it has no id or no published version (nothing to open or add). */
export function skillCard(skill: PublicSkillSummary): ListingCardData | null {
  const published = publishedVersionOf(skill);
  if (skill.id === undefined || published === undefined) return null;
  return {
    kind: 'skills',
    id: String(skill.id),
    name: skill.name ?? '',
    description: skill.description ?? '',
    iconUrl: undefined,
    likes: undefined,
    version: published.name,
    categories: skill.tags ?? [],
    recency: published.id,
  };
}

/** The listing page's URL parts for a card. */
export function listingParams(card: ListingCardData): { readonly kind: MarketplaceKind; readonly listingId: string } {
  return { kind: card.kind, listingId: card.id };
}

/** The listings that carry at least one of the picked categories; no pick keeps all of them (categories are multi-select for both kinds). */
export function filterByCategories(cards: readonly ListingCardData[], picked: readonly string[]): ListingCardData[] {
  if (picked.length === 0) return [...cards];
  return cards.filter((card) => card.categories.some((category) => picked.includes(category)));
}

/** Featured: the most recently published listings. There is no editorial flag yet, so recency is the stand-in (ADR-0051 M0). */
export function featuredRow(cards: readonly ListingCardData[]): ListingCardData[] {
  return [...cards].sort((a, b) => b.recency - a.recency).slice(0, FEATURED_COUNT);
}

/** Popular: the most liked listings that have any like; skills have no like store, so their row is empty. */
export function popularRow(cards: readonly ListingCardData[]): ListingCardData[] {
  return cards
    .filter((card) => (card.likes ?? 0) > 0)
    .sort((a, b) => (b.likes ?? 0) - (a.likes ?? 0))
    .slice(0, POPULAR_COUNT);
}
