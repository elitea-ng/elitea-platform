/**
 * Marketplace types — the one card shape both kinds of listing are drawn from.
 */

/** What a listing is: an agent to hire, a skill to add, or a saved team to hire (ADR-0051 amendment, issue 3137). Pipelines come after publishing supports them (ADR-0051 Q7). */
export type MarketplaceKind = 'agents' | 'skills' | 'teams';

/** One listing as a card shows it, whatever the kind. */
export interface ListingCardData {
  readonly kind: MarketplaceKind;
  /** The listing id: the page's URL key. */
  readonly id: string;
  /** The public catalogue agent or skill: likes and Try in chat use it. */
  readonly entityId: string;
  readonly name: string;
  readonly description: string;
  readonly iconUrl: string | undefined;
  readonly publisher: string | undefined;
  /** Null until reviews exist (ADR-0051 M5). */
  readonly ratingAvg: number | null;
  readonly ratingCount: number;
  readonly hires: number;
  readonly likes: number;
  /** The caller liked the public twin. */
  readonly isLiked: boolean;
  /** The current version's label. */
  readonly version: string | undefined;
  /** The current version's id: what Try on a skill card pins. */
  readonly versionId: string | undefined;
  readonly tier: string;
  readonly requiredTypes: readonly string[];
  /** The newest version this project hired, when it did. */
  readonly hiredVersion: string | undefined;
  readonly categories: readonly string[];
  readonly delisted: boolean;
}
