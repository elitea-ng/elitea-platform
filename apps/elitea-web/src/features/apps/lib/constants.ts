import { docsLink, resolveBrandPack } from '@/shared/brand';
import type { BrandPack } from '@/shared/brand';
import { t } from '@/shared/i18n';
import { WikiQueryIcon } from '@/shared/ui/icons/wiki-query-icon';
import { InventorySearchIcon } from '@/shared/ui/icons/inventory-search-icon';
import type { SvgIconComponent } from '@/shared/ui/icons/svg-icon.types';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/apps/lib/constants/applicationCatalog.constants.js`.
 *
 * The old app's "App Catalog" tab (`AppsTabs[1]`, `/apps/catalog`) is a
 * STATIC, hard-coded 2-item list — there is no server-side entity behind it
 * (confirmed against `entities/app`'s module doc: the marketplace `App`
 * type is a DIFFERENT concept, `PublicApplicationSummary`). This catalogue
 * therefore has no `entities/` type of its own; it is local, feature-owned
 * data, exactly as it was in the baseline.
 *
 * ADR-0024 decision 8 (WP8): every sub-application is a native screen
 * inside the brand provider, so the two things a tenant can still see of
 * the vendor are TEXT and LINKS. Both now come from outside this file:
 * copy through `t()` (`en.json`), documentation links through
 * `docsLink()` (the brand pack's `product.docsUrl`). The engine that backs
 * the Wikis entry is never named to a user — the entry is "Wikis", and the
 * theme gate (check 8) fails this file if the product string reappears.
 *
 * `applicationCatalog()` is a function, not a module constant, because
 * both inputs are resolved at call time: the served brand pack is read from
 * `window` (channel C), and the i18n bundle is what `t()` sees when the
 * caller runs. Callers memoise (`useApplicationCatalog`'s `useMemo`).
 */

/** One entry in the static App Catalog. */
export interface ApplicationCatalogEntry {
  /** The toolkit "type" key this catalogue entry configures (`ToolkitTypeSchemaMap` key). */
  readonly type: string;
  readonly name: string;
  readonly Icon: SvgIconComponent;
  readonly shortDescription: string;
  readonly description: string;
  readonly capabilities: readonly string[];
  readonly bestFor: string;
  readonly documentation: string;
}

export const REQUEST_STATUS = {
  NONE: 'none',
  PENDING: 'pending',
  APPROVED: 'approved',
  REJECTED: 'rejected',
} as const;

export type RequestStatus = (typeof REQUEST_STATUS)[keyof typeof REQUEST_STATUS];

/**
 * The catalogue's type keys, in entry order. A fixed-length module constant
 * with no brand or i18n input, so a hook that maps it to one query per entry
 * (`useModerationRequests`' `useQueries`) never becomes a variable-length
 * hook call.
 */
export const APPLICATION_CATALOG_TYPES = ['wikis_Wikis', 'inventory'] as const;

/**
 * Every type string a Wiki toolkit is stored under (`AdmittedWikisToolkitTypes` in
 * `internal/api/v2/deepwiki/wikis.go`): the catalogue key plus the spellings older toolkits carry.
 */
export const WIKI_TOOLKIT_TYPES: readonly string[] = ['wikis_Wikis', 'deepwiki_Deepwiki', 'wikis', 'deepwiki'];

/** The catalogue's two entries, in baseline order. `pack` defaults to the served brand pack. */
export function applicationCatalog(pack: BrandPack = resolveBrandPack()): readonly ApplicationCatalogEntry[] {
  return [
    {
      type: APPLICATION_CATALOG_TYPES[0],
      name: t('apps.catalog.wikis.name', 'Wikis'),
      Icon: WikiQueryIcon,
      shortDescription: t('apps.catalog.wikis.shortDescription', 'Generate searchable wiki pages from repository code.'),
      description: t(
        'apps.catalog.wikis.description',
        'Wikis turns a code repository into navigable documentation with architecture summaries, source-linked explanations, and project Q&A support.',
      ),
      capabilities: [
        t('apps.catalog.wikis.capabilities.generation', 'Wiki generation'),
        t('apps.catalog.wikis.capabilities.architecture', 'Architecture summaries'),
        t('apps.catalog.wikis.capabilities.qa', 'Code-aware Q&A'),
      ],
      bestFor: t('apps.catalog.wikis.bestFor', 'Onboarding, implementation context, and team knowledge'),
      documentation: docsLink('integrations/apps/wikis', pack),
    },
    {
      type: APPLICATION_CATALOG_TYPES[1],
      name: t('apps.catalog.inventory.name', 'Inventory'),
      Icon: InventorySearchIcon,
      shortDescription: t(
        'apps.catalog.inventory.shortDescription',
        'Explore services, ownership, dependencies, and repository landscape.',
      ),
      description: t(
        'apps.catalog.inventory.description',
        'Inventory helps teams inspect the code estate, map important components, and understand relationships before planning changes.',
      ),
      capabilities: [
        t('apps.catalog.inventory.capabilities.components', 'Component inventory'),
        t('apps.catalog.inventory.capabilities.dependencies', 'Dependency discovery'),
        t('apps.catalog.inventory.capabilities.ownership', 'Ownership context'),
      ],
      bestFor: t('apps.catalog.inventory.bestFor', 'Modernization, impact analysis, and engineering governance'),
      documentation: docsLink('integrations/apps/inventory', pack),
    },
  ];
}
