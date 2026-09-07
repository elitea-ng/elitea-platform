/**
 * One source's ingestion status, as a chip.
 *
 * Its own component because the mapping from a provider word to a colour and a
 * sentence is the part of this screen that is easy to get silently wrong, and
 * it is rendered in two places — the row and the ingestion banner.
 */
import Chip from '@mui/material/Chip';

import { t } from '@/shared/i18n';

import { sourceStatusView, type SourceStatusKind } from '../lib/sourceStatus';

export interface SourceStatusChipProps {
  /** The status word the provider reported, or '' for a source never ingested. */
  readonly status: string;
}

export function SourceStatusChip({ status }: SourceStatusChipProps): React.JSX.Element {
  const view = sourceStatusView(status);

  // A word this table has no reading for is printed AS IT STANDS. A newer
  // engine's status is information; showing it as "Not ingested" would report
  // the opposite of what the provider said.
  if (view === null) {
    return <Chip size="small" data-testid="inventory-source-status" label={status} />;
  }

  return (
    <Chip
      size="small"
      data-testid="inventory-source-status"
      data-status={view.kind}
      color={view.tone}
      label={labelFor(view.kind)}
    />
  );
}

/**
 * The kind's copy, as five LITERAL `t()` calls.
 *
 * A lookup table of keys would be shorter and would put every one of these
 * strings beyond `scripts/i18n-backfill.mjs`, which extracts literal call
 * sites only — the copy would render and never reach `en.json`.
 */
function labelFor(kind: SourceStatusKind): string {
  switch (kind) {
    case 'pending':
      return t('inventory.sources.status.pending', 'Pending');
    case 'ingesting':
      return t('inventory.sources.status.ingesting', 'Ingesting…');
    case 'done':
      return t('inventory.sources.status.done', 'Done');
    case 'error':
      return t('inventory.sources.status.error', 'Error');
    case 'waiting':
    default:
      return t('inventory.sources.status.waiting', 'Not ingested');
  }
}
