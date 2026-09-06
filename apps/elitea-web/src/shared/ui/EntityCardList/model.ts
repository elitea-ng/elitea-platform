/**
 * The row shape every list page (agents, pipelines, skills, toolkits, MCPs,
 * credentials, applications) hands to `EntityCardList`.
 *
 * Ported from the union of what the baseline's `components/Card.jsx` and
 * `[fsd]/widgets/data-table` read off a card row (`name`, `authors`/`author`,
 * `tags`, `icon_meta`, `created_at`) — narrowed to the fields the two views
 * actually render, and expressed as a view model the page maps its own wire
 * type into, so `shared/ui` stays free of any domain/API import.
 */
import type { ReactNode } from 'react';

/** One author avatar on a card's bottom row (`components/AuthorContainer.jsx`). */
export interface EntityListAuthor {
  readonly id?: string;
  readonly name: string;
  readonly avatar?: string;
}

/** One tag chip on a card's bottom row (`components/CardTagSection.jsx`). */
export interface EntityListTag {
  readonly id?: string | number;
  readonly name: string;
}

/** @public One card/row. Pages build these from their own wire rows. */
export interface EntityListItem {
  readonly id: string;
  readonly name: string;
  readonly description?: string;
  /** The entity-type glyph inside the round gradient tile (`components/EntityIcon.jsx`). */
  readonly icon?: ReactNode;
  readonly authors?: readonly EntityListAuthor[];
  readonly tags?: readonly EntityListTag[];
  /** ISO timestamp — the table view's "Created" column. */
  readonly createdAt?: string;
  /** Table view's "Type" column, and the card's bottom row when the domain has no tags (credentials/toolkits). */
  readonly typeLabel?: string;
  readonly onClick?: () => void;
}

/** `ViewOptions` (`apps/elitea-ui/src/common/constants.js`) — the `?view=` search-param vocabulary. */
export const ENTITY_LIST_VIEW = { cards: 'cards', table: 'table' } as const;

/** @public The two list renderings `EntityCardList` switches between. */
export type EntityListView = (typeof ENTITY_LIST_VIEW)[keyof typeof ENTITY_LIST_VIEW];
