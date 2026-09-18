/**
 * Hardcoded fallback configurations for resource cards.
 *
 * KEY DECISION #2 (issue #26): the Go endpoints
 * `GET /admin/system_info/prompt_lib` and
 * `GET /admin/plugin_config_values/prompt_lib/resources` exist but lack
 * OpenAPI specs in the new app. **Render page using these hardcoded
 * defaults — do NOT fake network calls.** Document the gap above.
 *
 * These values match the old `resources/index.jsx` baseline exactly.
 * They are the source-of-truth when the admin config endpoints are not
 * available or return empty.
 *
 * See `./useResourcesConfig.ts` for the full explanation of the API gap
 * and exactly what unblocks it.
 */

import type { ResourceCardConfig, ResourceDefaultLink } from './ResourceCard.types';

// Icon imports — shared/ui icons that already exist in the new app.
// The old app imports from `@/assets/*.svg?react`; these are Wave-1
// equivalents available at shared/ui/icons/.
import { FileIcon } from '@/shared/ui/icons/file-icon';
import { RocketIcon } from '@/shared/ui/icons/rocket-icon';
import { TutorialsIcon } from '@/shared/ui/icons/tutorials-icon';
import { VideoIcon } from '@/shared/ui/icons/video-icon';

// Tour target ids — ported from interactive-tours constants (already built).
import { RESOURCES_TOUR_TARGET_IDS } from '@/features/interactive-tours';
import { docsLink } from '@/shared/brand';

/**
 * #891 (issues 890-891, ELITEA-0967/0972/0973/0975): every card's link list
 * was 100% admin-configured with no default, so a fresh deployment (or any
 * deployment whose admin never visited Admin > Features > Help Center)
 * showed "No links configured" everywhere. These defaults point at REAL,
 * shipped pages of the embedded docs SPA (`src/entries/docs/content/**`,
 * served under the brand pack's `product.docsUrl`, `/docs/` by default —
 * `docsLink()`'s own header) — verified against that content tree's actual
 * file slugs (`content-registry.ts`'s `slugFromPath`), not invented paths.
 *
 * Resolved with the DEFAULT brand pack (no `pack` argument): unlike a
 * component, this is a plain module constant evaluated once at import time,
 * so it cannot re-resolve channel C (a served, per-deployment pack) the way
 * `applicationCatalog(pack)` does. A deployment that reassigns `docsUrl`
 * keeps a working link (still resolved through `docsLink`, never a literal
 * `docs.elitea.ai`) — only the very rare mid-session pack change would not
 * retarget these specific defaults, an acceptable trade for cards that are
 * admin-overridable anyway.
 *
 * Video Library and Interactive Tours carry NO `defaultLinks`: this app
 * hosts no product-walkthrough VIDEO content anywhere, and inventing four
 * "video" links pointing at text documentation would misrepresent the
 * medium — the same reasoning issue #892 (this file's sibling gap) already
 * applied to the hardcoded plugin-version list. Left as a real, open gap
 * (`shell.resources.spec.ts`'s RES10 stays `test.fail`-marked).
 */
const DOCUMENTATION_LINKS: ReadonlyArray<ResourceDefaultLink> = [
  { title: 'Getting Started', url: docsLink('getting-started/install') },
  { title: 'How-To Guides', url: docsLink('how-tos/agents-pipelines/build-agent-with-ai') },
  { title: 'Integrations', url: docsLink('integrations/third-party-integrations/api-usage') },
  { title: 'Migration & Update', url: docsLink('what-is-new') },
];

const TUTORIALS_LINKS: ReadonlyArray<ResourceDefaultLink> = [
  { title: 'Chat Quick Start', url: docsLink('getting-started/chat-quick-start') },
  { title: 'Configure an AI Provider', url: docsLink('getting-started/configure-ai-provider') },
  { title: 'Connect Toolkits', url: docsLink('getting-started/connect-toolkits-quick-start') },
  // The manual case's "More…" — the docs home, the nearest real "see every
  // guide" destination this app has (no directory-index page exists per
  // category; `docsLink('')` resolves to `content/index.mdx`).
  { title: 'More…', url: docsLink('') },
];

const RELEASE_NOTES_LINKS: ReadonlyArray<ResourceDefaultLink> = [
  // One real entry with the "Latest" badge (#891/ELITEA-0975) — the
  // embedded docs ship a single living "What's new" page, not a list of
  // past-release entries (see that page's own header), so there is no
  // historical entry to mark AS historical; the case's own claim ("marks
  // the latest entry over historical releases") is satisfied for the one
  // entry this app actually has.
  { title: "What's New", url: docsLink('what-is-new'), badge: 'Latest' },
];

/** Default resource card configurations consumed by the HelpCenterPage. */
export const RESOURCE_CARD_CONFIGS: ReadonlyArray<ResourceCardConfig> = [
  {
    enabledKey: 'resources_documentation_enabled',
    titleKey: 'resources_documentation_title',
    descriptionKey: 'resources_documentation_description',
    defaultTitle: 'Documentation',
    defaultDescription: 'API reference, guides, and platform concepts',
    Icon: FileIcon,
    linksKey: 'resources_documentation_links',
    colorScheme: 'blue',
    tourTargetId: RESOURCES_TOUR_TARGET_IDS.documentationCard,
    defaultLinks: DOCUMENTATION_LINKS,
  },
  {
    enabledKey: 'resources_release_notes_enabled',
    titleKey: 'resources_release_notes_title',
    descriptionKey: 'resources_release_notes_description',
    defaultTitle: 'Release Notes',
    defaultDescription: 'Product updates, improvements, and fixes',
    Icon: RocketIcon,
    linksKey: 'resources_release_notes_links',
    colorScheme: 'orange',
    tourTargetId: RESOURCES_TOUR_TARGET_IDS.releaseNotesCard,
    defaultLinks: RELEASE_NOTES_LINKS,
  },
  {
    enabledKey: 'resources_video_library_enabled',
    titleKey: 'resources_video_library_title',
    descriptionKey: 'resources_video_library_description',
    defaultTitle: 'Video Library',
    defaultDescription: 'Product walkthroughs and recorded sessions',
    Icon: VideoIcon,
    linksKey: 'resources_video_library_links',
    colorScheme: 'purple',
    tourTargetId: RESOURCES_TOUR_TARGET_IDS.videoLibraryCard,
  },
  {
    enabledKey: 'resources_tutorials_enabled',
    titleKey: 'resources_tutorials_title',
    descriptionKey: 'resources_tutorials_description',
    defaultTitle: 'Tutorials',
    defaultDescription: 'Step-by-step guides and use cases',
    Icon: TutorialsIcon,
    linksKey: 'resources_tutorials_links',
    colorScheme: 'green',
    tourTargetId: RESOURCES_TOUR_TARGET_IDS.tutorialsCard,
    defaultLinks: TUTORIALS_LINKS,
  },
  {
    enabledKey: 'resources_interactive_tours_enabled',
    titleKey: 'resources_interactive_tours_title',
    descriptionKey: 'resources_interactive_tours_description',
    defaultTitle: 'Interactive Tours',
    defaultDescription: 'Guided tours to explore key features and workflows',
    Icon: VideoIcon,
    linksKey: 'resources_interactive_tours_links',
    colorScheme: 'pink',
    tourTargetId: RESOURCES_TOUR_TARGET_IDS.interactiveToursCard,
  },
];
