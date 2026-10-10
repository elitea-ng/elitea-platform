/**
 * A workspace's local index at a glance: Off / Building n/m / Ready · N
 * entities / Stale / Error. Clicking it opens the index settings.
 */
import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import Chip from '@mui/material/Chip';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import { describeIndexError } from '../model/describeIndexError';
import type { IndexProgress } from '../model/progress';
import type { IndexView } from '../model/useWorkspaceIndex';

export interface IndexStatusChipProps {
  view: IndexView | undefined;
  loadError: unknown;
  progress: IndexProgress | null;
  onClick: () => void;
}

type ChipColor = 'default' | 'success' | 'warning' | 'error' | 'info';

const compact = (value: number): string => new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(value);

type ChipLook = { label: string; color: ChipColor; hint: string };

/** Before there is a status: reading it, it could not be read, or the policy turns the index off. */
function describeNoStatus(view: IndexView | undefined, loadError: unknown): ChipLook {
  if (view?.kind === 'disabled') {
    return { label: t('workspace.index.chip.off', 'Off'), color: 'default', hint: t('workspace.index.policyOff', 'The local code index is turned off by your organisation’s policy.') };
  }
  if (loadError === null || loadError === undefined) return { label: t('workspace.index.chip.reading', 'Index…'), color: 'default', hint: t('workspace.index.chip.loading', 'Reading the code index') };
  return { label: t('workspace.index.chip.error', 'Error'), color: 'error', hint: describeIndexError(loadError) };
}

/** The label, colour and longer explanation for one view of the index. */
function describeChip(view: IndexView | undefined, loadError: unknown, progress: IndexProgress | null): ChipLook {
  if (view?.kind !== 'status') return describeNoStatus(view, loadError);
  const { status } = view;
  switch (status.state) {
    case 'off':
      return { label: t('workspace.index.chip.off', 'Off'), color: 'default', hint: t('workspace.index.chip.offHint', 'The code index is off for this folder.') };
    case 'building':
      return {
        label:
          progress === null || progress.total === null
            ? t('workspace.index.chip.building', 'Building…')
            : t('workspace.index.chip.buildingCount', 'Building {{done}}/{{total}}', { done: String(progress.done), total: String(progress.total) }),
        color: 'info',
        hint: t('workspace.index.chip.buildingHint', 'Indexing the code structure of this folder.'),
      };
    case 'ready':
      return {
        label: t('workspace.index.chip.ready', 'Ready · {{entities}} entities', { entities: compact(status.entities) }),
        color: 'success',
        hint: t('workspace.index.chip.readyHint', 'Agents can search this folder’s code structure.'),
      };
    case 'stale':
      return {
        label: t('workspace.index.chip.stale', 'Stale'),
        color: 'warning',
        hint: t('workspace.index.chip.staleHint', 'Files changed since the last build; it refreshes when it is next used.'),
      };
    case 'error':
      return { label: t('workspace.index.chip.error', 'Error'), color: 'error', hint: status.error ?? t('workspace.index.chip.errorHint', 'The last build failed.') };
  }
}

export function IndexStatusChip({ view, loadError, progress, onClick }: IndexStatusChipProps): React.JSX.Element {
  const { label, color, hint } = describeChip(view, loadError, progress);
  return (
    <Tooltip title={hint}>
      <Chip
        size="small"
        variant="outlined"
        color={color}
        icon={<AccountTreeOutlinedIcon fontSize="inherit" />}
        label={label}
        onClick={onClick}
        data-testid="index-status-chip"
        aria-label={t('workspace.index.chip.label', 'Code index: {{status}}', { status: label })}
      />
    </Tooltip>
  );
}
