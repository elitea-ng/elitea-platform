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

function optionsOf(counts: Readonly<Record<string, number>> | undefined, order: readonly string[], label: (value: string) => string): WorkshopFacet['options'] {
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
    return t('features.agentLifecycle.badge.forkedFrom', 'Forked from {{listing}} {{version}}', { listing, version }).trim();
  }
  return '';
}

/**
 * The words a row's badge carries: the status, then the origin, then (for the
 * second hire of one listing) the name of the first hire. Text, so a screen
 * reader hears it and colour is not the only signal.
 */
export function workshopRowBadge(application: Application, rows: readonly Application[]): string | undefined {
  const parts = [statusBadgeLabel(application.status)];
  const origin = originBadgeLabel(application);
  if (origin !== '') parts.push(origin);
  const listingId = metaText(application, 'origin_listing_id');
  if (listingId !== '') {
    const first = rows.find((row) => metaText(row, 'origin_listing_id') === listingId);
    if (first !== undefined && first.id !== application.id && rows.indexOf(first) < rows.indexOf(application)) {
      parts.push(t('features.agentLifecycle.badge.alsoHiredAs', 'also hired as {{name}}', { name: first.name }));
    }
  }
  const text = parts.filter((part): part is string => part !== undefined && part !== '').join(' · ');
  return text === '' ? undefined : text;
}
