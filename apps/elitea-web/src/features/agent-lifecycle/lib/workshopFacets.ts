import type { Application, ApplicationListFacets } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The status and origin filters of the Workshop's agents and pipelines lists
 * (ADR-0051), from the counts the list response carries. Only a value that
 * occurs is a key of those counts, so the three statuses no agent can hold
 * (moderation, approval, rejected) are never offered; the order is the one
 * a person reads left to right through an agent's life.
 */
interface WorkshopFacet {
  /** The URL search param the choice lives in. */
  readonly param: 'statuses' | 'origin' | 'update_available';
  readonly array?: boolean;
  readonly label: string;
  readonly chipLabel: string;
  readonly options: readonly { readonly value: string; readonly label: string; readonly count: number }[];
}

const STATUS_ORDER = ['draft', 'published'] as const;
const ORIGIN_ORDER = ['own', 'installed', 'forked'] as const;

function statusLabel(value: string): string {
  if (value === 'draft') return t('features.agentLifecycle.facets.statusDraft', 'Draft');
  if (value === 'published') return t('features.agentLifecycle.facets.statusPublished', 'Published');
  return value;
}

function originLabel(value: string): string {
  if (value === 'own') return t('features.agentLifecycle.facets.originOwn', 'Own');
  if (value === 'installed') return t('features.agentLifecycle.facets.originInstalled', 'Installed');
  if (value === 'forked') return t('features.agentLifecycle.facets.originForked', 'Forked');
  return value;
}

function optionsOf(
  counts: Readonly<Record<string, number>> | undefined,
  order: readonly string[],
  label: (value: string) => string,
): WorkshopFacet['options'] {
  const known = order.filter((value) => (counts?.[value] ?? 0) > 0);
  const others = Object.keys(counts ?? {}).filter((value) => !order.includes(value) && (counts?.[value] ?? 0) > 0);
  return [...known, ...others].map((value) => ({ value, label: label(value), count: counts?.[value] ?? 0 }));
}

export function workshopFacets(facets: ApplicationListFacets | undefined): readonly WorkshopFacet[] {
  return [
    {
      param: 'statuses',
      array: true,
      label: t('features.agentLifecycle.facets.status', 'Status'),
      chipLabel: t('features.agentLifecycle.facets.status', 'Status'),
      options: optionsOf(facets?.statuses, STATUS_ORDER, statusLabel),
    },
    {
      param: 'origin',
      label: t('features.agentLifecycle.facets.origin', 'Origin'),
      chipLabel: t('features.agentLifecycle.facets.origin', 'Origin'),
      options: optionsOf(facets?.origins, ORIGIN_ORDER, originLabel),
    },
    {
      param: 'update_available',
      label: t('features.agentLifecycle.facets.updates', 'Updates'),
      chipLabel: t('features.agentLifecycle.facets.updates', 'Updates'),
      options: optionsOf({ true: facets?.update_available ?? 0 }, ['true'], () =>
        t('features.agentLifecycle.facets.updateAvailable', 'Update available'),
      ),
    },
  ];
}

/** The status badge a row carries: its text, so it is announced and not only coloured. */
export function statusBadgeLabel(status: string | undefined): string | undefined {
  return status === undefined || status === '' ? undefined : statusLabel(status);
}

function metaText(application: Application, key: string): string {
  const value = application.meta?.[key];
  return typeof value === 'string' || typeof value === 'number' ? String(value) : '';
}

/** The origin a hired row shows: "Installed v2.3", or "Forked from Listing v1.2". Empty for an agent the project owns. */
export function originBadgeLabel(application: Application): string {
  const kind = metaText(application, 'origin_kind');
  const version = metaText(application, 'origin_version_name');
  const listing = metaText(application, 'origin_listing_name');
  if (kind === 'installed') {
    return version === ''
      ? t('features.agentLifecycle.badge.installed', 'Installed')
      : t('features.agentLifecycle.badge.installedVersion', 'Installed {{version}}', { version });
  }
  if (kind === 'forked' && listing !== '') {
    return t('features.agentLifecycle.badge.forkedFrom', 'Forked from {{listing}} {{version}}', {
      listing,
      version,
    }).trim();
  }
  return '';
}

/**
 * The first loaded row of each listing, in list order. One pass per rows array,
 * kept by array identity, so a list of N rows costs O(N) and not O(N squared)
 * (the badge is asked once per row).
 */
const firstHireCache = new WeakMap<readonly Application[], Map<string, Application>>();

function firstHireByListing(rows: readonly Application[]): Map<string, Application> {
  const cached = firstHireCache.get(rows);
  if (cached !== undefined) return cached;
  const first = new Map<string, Application>();
  for (const row of rows) {
    const listingId = metaText(row, 'origin_listing_id');
    if (listingId !== '' && !first.has(listingId)) first.set(listingId, row);
  }
  firstHireCache.set(rows, first);
  return first;
}

/**
 * The words a row's badge carries: the status, then the origin, then (for the
 * second hire of one listing) the name of the first hire. Text, so a screen
 * reader hears it and colour is not the only signal.
 *
 * "Also hired as" is computed from the rows LOADED in the list (the page in
 * hand), not from the whole project: a first hire on another page is not seen.
 */
export function workshopRowBadge(application: Application, rows: readonly Application[]): string | undefined {
  const parts = [statusBadgeLabel(application.status)];
  const origin = originBadgeLabel(application);
  if (origin !== '') parts.push(origin);
  // An agent the workforce policy hired stays on probation until a person keeps it (ADR-0051 decision 6).
  if (application.meta?.['on_probation'] === true) {
    parts.push(t('features.agentLifecycle.badge.onProbation', 'On probation'));
  }
  const listingId = metaText(application, 'origin_listing_id');
  const first = listingId === '' ? undefined : firstHireByListing(rows).get(listingId);
  if (first !== undefined && first.id !== application.id) {
    parts.push(t('features.agentLifecycle.badge.alsoHiredAs', 'also hired as {{name}}', { name: first.name }));
  }
  const text = parts.filter((part): part is string => part !== undefined && part !== '').join(' · ');
  return text === '' ? undefined : text;
}
