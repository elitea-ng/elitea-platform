/**
 * Marketplace types — the agent row the public catalogue serves, and the one
 * card shape both kinds of listing are drawn from.
 */
import type { PublicApplicationSummary } from '@/shared/api/generated/model';

/**
 * `tags`, `likes` and `is_liked` are NOT redeclared here: the catalogue row
 * carries all three (`PublicApplicationSummary` in the generated model), so an
 * optional copy on this interface would let a card read `undefined` from a
 * field the server always sends.
 */
export interface ApplicationData extends PublicApplicationSummary {
  category?: string;
  welcome_message?: string;
  conversation_starters?: string[];
}

/** What a listing is: an agent to hire or a skill to add. Pipelines come after publishing supports them (ADR-0051 Q7). */
export type MarketplaceKind = 'agents' | 'skills';

/** One listing as a card shows it, whatever the kind. */
export interface ListingCardData {
  readonly kind: MarketplaceKind;
  readonly id: string;
  readonly name: string;
  readonly description: string;
  readonly iconUrl: string | undefined;
  /** Agents only: the catalogue's one quality signal. Skills have no like store. */
  readonly likes: number | undefined;
  /** The current published version's label, when the row carries one. */
  readonly version: string | undefined;
  /** The categories the listing is filed under: the agent's one category, a skill's tags. */
  readonly categories: readonly string[];
}

/**
 * The sort keys the agent catalogue accepts. Anything else is a 400 from the
 * handler, so this union is the server's allowlist, not a wish
 * (`services/elitea-main/internal/api/v2/eliteacore/public_applications.go`).
 */
export type MarketplaceSortBy = 'created_at' | 'name' | 'likes';
export type MarketplaceSortOrder = 'asc' | 'desc';
