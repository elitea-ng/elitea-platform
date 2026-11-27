/**
 * A workspace's local folder index at a glance: Off / Building n/m / Ready ·
 * N entities (· M documents, once PDF or Office files are indexed; ·
 * Watching, while the host watches the folder) / Stale / Policy changed /
 * Error. Clicking it opens the index settings.
 */
import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import Chip from '@mui/material/Chip';
import Tooltip from '@mui/material/Tooltip';

import type { IndexStatus } from '@/shared/desktop/indexIpc';
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
    return { label: t('workspace.index.chip.off', 'Off'), color: 'default', hint: t('workspace.index.policyOff', 'The local folder index is turned off by your organisation’s policy.') };
  }
  if (loadError === null || loadError === undefined) return { label: t('workspace.index.chip.reading', 'Index…'), color: 'default', hint: t('workspace.index.chip.loading', 'Reading the folder index') };
  return { label: t('workspace.index.chip.error', 'Error'), color: 'error', hint: describeIndexError(loadError) };
}

/** When a stale index refreshes: after the agent's changes (or any change, while the folder is watched), or when the folder is opened. */
function staleHint(status: IndexStatus): string {
  if (status.watching) {
    return t('workspace.index.chip.staleWatchingHint', 'Files changed since the last build. The folder is watched: it refreshes a few seconds after files stop changing, or when you refresh it.');
  }
  return status.changed_files > 0
    ? t('workspace.index.chip.staleChangedHint', 'Files changed since the last build. It refreshes a few seconds after the agent’s changes, or when you refresh it.')
    : t('workspace.index.chip.staleHint', 'Not checked since the app started. It is checked when you open this folder, or when you refresh it.');
}

/** A ready chip's label: its entities, the PDF and Office files indexed (when any), and whether the folder is watched. */
function readyLabel(status: IndexStatus): string {
  const entities = compact(status.entities);
  const count = status.documents ?? 0;
  if (count > 0) {
    const documents = compact(count);
    return status.watching
      ? t('workspace.index.chip.readyDocumentsWatching', 'Ready · {{entities}} entities · {{documents}} documents · Watching', { entities, documents })
      : t('workspace.index.chip.readyDocuments', 'Ready · {{entities}} entities · {{documents}} documents', { entities, documents });
  }
  return status.watching
    ? t('workspace.index.chip.readyWatching', 'Ready · {{entities}} entities · Watching', { entities })
    : t('workspace.index.chip.ready', 'Ready · {{entities}} entities', { entities });
}

/** A ready index, and whether the host watches its folder. */
function readyChip(status: IndexStatus): ChipLook {
  const label = readyLabel(status);
  return status.watching
    ? {
        label,
        color: 'success',
        hint: t('workspace.index.chip.readyWatchingHint', 'Agents can search this folder’s code and documents. The folder is watched: changes made outside the app refresh it a few seconds after they stop.'),
      }
    : {
        label,
        color: 'success',
        hint: t('workspace.index.chip.readyHint', 'Agents can search this folder’s code and documents.'),
      };
}

/** The label, colour and longer explanation for one view of the index. */
function describeChip(view: IndexView | undefined, loadError: unknown, progress: IndexProgress | null): ChipLook {
  if (view?.kind !== 'status') return describeNoStatus(view, loadError);
  const { status } = view;
  switch (status.state) {
    case 'off':
      return { label: t('workspace.index.chip.off', 'Off'), color: 'default', hint: t('workspace.index.chip.offHint', 'The folder index is off for this folder.') };
    case 'building':
      return {
        label:
          progress === null || progress.total === null
            ? t('workspace.index.chip.building', 'Building…')
            : t('workspace.index.chip.buildingCount', 'Building {{done}}/{{total}}', { done: String(progress.done), total: String(progress.total) }),
        color: 'info',
        hint: t('workspace.index.chip.buildingHint', 'Indexing this folder’s code and documents.'),
      };
    case 'ready':
      return readyChip(status);
    case 'stale':
      return { label: t('workspace.index.chip.stale', 'Stale'), color: 'warning', hint: staleHint(status) };
    case 'stale_policy':
      return {
        label: t('workspace.index.chip.stalePolicy', 'Policy changed'),
        color: 'warning',
        hint: t('workspace.index.chip.stalePolicyHint', 'Your organisation’s file access rules changed. Agents get the index again once it is rebuilt under them, when you open this folder.'),
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
        aria-label={t('workspace.index.chip.label', 'Folder index: {{status}}', { status: label })}
      />
    </Tooltip>
  );
}
