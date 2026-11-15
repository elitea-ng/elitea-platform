import type { ApplicationListFacets } from '@/shared/api/generated/model';
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
  readonly param: 'statuses' | 'origin';
  readonly array?: boolean;
  readonly label: string;
  readonly chipLabel: string;
  readonly options: readonly { readonly value: string; readonly label: string; readonly count: number }[];
}

const STATUS_ORDER = ['draft', 'published'] as const;
const ORIGIN_ORDER = ['own', 'forked'] as const;

function statusLabel(value: string): string {
  if (value === 'draft') return t('features.agentLifecycle.facets.statusDraft', 'Draft');
  if (value === 'published') return t('features.agentLifecycle.facets.statusPublished', 'Published');
  return value;
}

function originLabel(value: string): string {
  if (value === 'own') return t('features.agentLifecycle.facets.originOwn', 'Own');
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
  ];
}

/** The status badge a row carries: its text, so it is announced and not only coloured. */
export function statusBadgeLabel(status: string | undefined): string | undefined {
  return status === undefined || status === '' ? undefined : statusLabel(status);
}
