/**
 * The folder's index as one quiet line (the composer footer icon's tooltip): "Index ready · 12.4K entities", "Index stale". `null` when there
 * is nothing worth saying (off, still reading, building — the island shows a
 * build itself) or without a host. Reads the same query the page's settings
 * dialog does, so it costs no extra request.
 */
import { t } from '@/shared/i18n';

import type { IndexView } from './useWorkspaceIndex';

const compact = (value: number): string => new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(value);

export function indexLine(view: IndexView | undefined): string | null {
  if (view?.kind !== 'status') return null;
  const { status } = view;
  switch (status.state) {
    case 'ready':
      return t('desktop.island.indexReady', 'Index ready · {{entities}} entities', { entities: compact(status.entities) });
    case 'stale':
    case 'stale_policy':
      return t('desktop.island.indexStale', 'Index stale');
    case 'error':
      return t('desktop.island.indexError', 'Index error');
    case 'building':
    case 'off':
      return null;
  }
}
