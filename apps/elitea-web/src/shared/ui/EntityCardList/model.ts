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
  /** The author's account was deleted (#6702): draw a placeholder avatar, not initials. */
  readonly deleted?: boolean;
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
  /** The row's status as a word ("Draft", "Published"): a badge on the card and a Status column in the table. Text, so it is announced, not only coloured. */
  readonly badge?: string;
  /**
   * #915 — when set, both card and table views render a "Forked from" link
   * beside the entity's name that navigates to the SOURCE entity instead of
   * this one (`onClick`, above). Only the agent/pipeline list pages supply
   * this today (`Application.is_forked`/`meta.parent_entity_id`, the same
   * pair `ApplicationInformation.tsx`'s own "Forked from" row already reads,
   * now surfaced on the LIST row too — see repos/applications.go's List).
   */
  readonly forkedFrom?: { readonly onClick: () => void };
  /**
   * One extra action on the row, offered in the TABLE view only: a card is a
   * widget, and a control nested in it fails axe's `nested-interactive`
   * (the reason `forkedFrom` is a marker on the card).
   */
  readonly action?: { readonly label: string; readonly onClick: () => void };
  /** The rich card's second line (an agent's persona role, #3133). */
  readonly subtitle?: string;
  /** ISO timestamp of the last change: the rich card's muted "Updated … ago" line. */
  readonly updatedAt?: string;
  /** The rich card's primary action (an agent's Chat), drawn on hover and focus beside the row slot's controls. */
  readonly primaryAction?: { readonly label: string; readonly onClick: () => void; readonly disabled?: boolean };
}

/** `ViewOptions` (`apps/elitea-ui/src/common/constants.js`) — the `?view=` search-param vocabulary. */
export const ENTITY_LIST_VIEW = { cards: 'cards', table: 'table' } as const;

/** @public The two list renderings `EntityCardList` switches between. */
export type EntityListView = (typeof ENTITY_LIST_VIEW)[keyof typeof ENTITY_LIST_VIEW];

/**
 * The card a grid draws: `compact` is the port of the old app's card (every
 * list but Agents), `rich` the Agents card of issue 3133 (avatar, description,
 * persona, status beside the name, actions on hover and focus).
 */
export type EntityCardVariant = 'compact' | 'rich';
