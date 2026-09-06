import { t } from '@/shared/i18n';

/** The four sortable columns of the artifacts table (`ArtifactTable.jsx`'s `ARTIFACT_COLUMNS`). */
export type ArtifactSortField = 'name' | 'fileType' | 'size' | 'lastModified';

export interface ArtifactColumn {
  readonly field: ArtifactSortField | 'actions';
  readonly label: string;
  readonly sortable: boolean;
  /** Dropped when the table is narrower than this, in px (baseline `hideBelow`). */
  readonly hideBelow?: number;
}

const CHECKBOX_COLUMN = '3rem';
const ACTIONS_COLUMN = '7rem';

/**
 * The baseline's column table, including its `hideBelow` ladder — the reason
 * the header and the rows can share one `gridTemplateColumns` string and stay
 * aligned as columns drop out. Built per call because every label goes through
 * `t()`, which resolves against the loaded bundle, not at module load.
 */
function artifactColumns(): readonly ArtifactColumn[] {
  return [
    { field: 'name', label: t('common.name', 'Name'), sortable: true },
    { field: 'fileType', label: t('artifacts.table.type', 'Type'), sortable: true, hideBelow: 550 },
    { field: 'size', label: t('common.size', 'Size'), sortable: true, hideBelow: 700 },
    { field: 'lastModified', label: t('artifacts.table.lastUpdate', 'Last update'), sortable: true, hideBelow: 900 },
    { field: 'actions', label: t('common.actions', 'Actions'), sortable: false },
  ];
}

export function visibleArtifactColumns(width: number): readonly ArtifactColumn[] {
  return artifactColumns().filter((column) => column.hideBelow === undefined || width >= column.hideBelow);
}

/** `3rem` for the checkbox, `1fr` per data column, `7rem` for the actions column. */
export function artifactGridTemplate(columns: readonly ArtifactColumn[]): string {
  return [
    CHECKBOX_COLUMN,
    ...columns.map((column) => (column.field === 'actions' ? ACTIONS_COLUMN : '1fr')),
  ].join(' ');
}
