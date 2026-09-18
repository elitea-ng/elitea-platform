/**
 * TypeScript types for the help-center ResourceCard component.
 * Ported from `apps/elitea-ui/src/[fsd]/pages/resources/ui/ResourceCard.jsx`.
 */

import type { ReactNode } from 'react';

/** Named color schemes matching the `resourceCard` palette tokens. */
type ResourceColorScheme = 'blue' | 'orange' | 'purple' | 'green' | 'pink';

/**
 * A card's DEFAULT link (#891/ELITEA-0972,0973,0974,0975) — shown until an
 * administrator configures the card's `linksKey` (`resolveLinks`,
 * `HelpCenterPage.tsx`). Every URL here points at REAL, shipped content
 * (the embedded docs SPA, `src/entries/docs/content/**`) — no invented
 * "coming soon" links. `badge` is the one additional concept the Release
 * Notes case needs ("Latest" over historical entries); no other card uses it.
 */
export interface ResourceDefaultLink {
  readonly title: string;
  readonly url: string;
  readonly badge?: string;
}

/** Configuration entry for a single resource card. */
export interface ResourceCardConfig {
  /** Unique key used for React `key` and to look up config values from the API response. */
  readonly enabledKey: string;
  /** i18n / API key for the card title. */
  readonly titleKey: string;
  /** i18n / API key for the card description. */
  readonly descriptionKey: string;
  /** Shown when the API has not yet returned a title. */
  readonly defaultTitle: string;
  /** Shown when the API has not yet returned a description. */
  readonly defaultDescription: string;
  /** MUI icon element rendered in the card header. */
  readonly Icon: React.ComponentType<{ width?: string; height?: string }>;
  /** API key for the card's link array. */
  readonly linksKey: string;
  /** Color scheme token mapped to `resourceCard.<scheme>` palette entries. */
  readonly colorScheme: ResourceColorScheme;
  /** Tour target id consumed by the interactive-tours feature. */
  readonly tourTargetId: string;
  /** Shown until an admin configures `linksKey` themselves — see `ResourceDefaultLink`. Omitted entirely on a card with no real content to default to (#891's Video Library gap; see that card's own comment). */
  readonly defaultLinks?: ReadonlyArray<ResourceDefaultLink>;
}

/** Props passed to the ResourceCard component. */
export interface ResourceCardProps {
  title: string;
  description: string;
  colorScheme: ResourceColorScheme;
  tourTargetId: string;
  /** Icon element rendered in the card header. */
  icon: ReactNode;
  /** Children — typically link items or a "no links" message. */
  children: ReactNode;
}
