import type { ChangeEvent, DragEvent, ReactNode } from 'react';
import { useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { format } from 'date-fns';

import { formatArtifactSize, type Artifact } from '@/entities/artifact';
import { t } from '@/shared/i18n';

import { getItemsAtCurrentLevel, parsePrefixToBreadcrumbs } from '../lib/fileTree';
import { getFileTypeName } from '../lib/fileTypeName';
import { useElementWidth } from '../lib/useElementWidth';
import { artifactGridTemplate, visibleArtifactColumns, type ArtifactSortField } from '../model/columns';
import type { ArtifactListItem } from '../model/types';
import { ArtifactGridHeader } from './ArtifactGridHeader';
import { ArtifactGridRow } from './ArtifactGridRow';
import { ArtifactPagination } from './ArtifactPagination';
import { ArtifactTableEmpty } from './ArtifactTableEmpty';
import { ArtifactTableToolbar } from './ArtifactTableToolbar';

/** `ArtifactTable.jsx`'s `DATE_FORMAT` — 12-hour, day-first, never the locale's own. */
const DATE_FORMAT = 'dd-MM-yyyy, hh:mm a';

interface ArtifactTableProps {
  readonly bucket: string;
  readonly retentionDays: number | null;
  readonly contents: readonly Artifact[];
  readonly currentPrefix: string;
  readonly loading: boolean;
  readonly error?: string;
  readonly onPrefixChange: (prefix: string) => void;
  readonly onPreview: (item: ArtifactListItem) => void;
  readonly onDownload: (item: ArtifactListItem) => void;
  readonly onDownloadMany: (items: readonly ArtifactListItem[]) => void;
  readonly onDelete: (items: readonly ArtifactListItem[]) => void;
  readonly onUpload: (files: readonly File[]) => void;
}

function compare(left: ArtifactListItem, right: ArtifactListItem, field: ArtifactSortField): number {
  if (field === 'size') return left.size - right.size;
  if (field === 'lastModified') return (left.lastModified ?? '').localeCompare(right.lastModified ?? '');
  if (field === 'fileType') return getFileTypeName(left.name).localeCompare(getFileTypeName(right.name));
  return left.name.localeCompare(right.name);
}

/** Folders first, then the active sort — `sortedRows` in the baseline. */
function sortItems(
  items: readonly ArtifactListItem[],
  field: ArtifactSortField,
  direction: 'asc' | 'desc',
): ArtifactListItem[] {
  const factor = direction === 'asc' ? 1 : -1;
  return [...items].sort((left, right) => {
    if (left.kind !== right.kind) return left.kind === 'folder' ? -1 : 1;
    return compare(left, right, field) * factor;
  });
}

function cellValues(item: ArtifactListItem): Record<string, string> {
  if (item.kind === 'folder') return { fileType: '-', size: '-', lastModified: '-' };
  return {
    fileType: getFileTypeName(item.name),
    size: formatArtifactSize(item.size),
    lastModified: item.lastModified === undefined ? '-' : format(new Date(item.lastModified), DATE_FORMAT),
  };
}

// oxlint-disable-next-line complexity -- one table: selection, sorting, pagination and the drop target, exactly as the baseline composes them.
export function ArtifactTable(props: ArtifactTableProps): ReactNode {
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [sortField, setSortField] = useState<ArtifactSortField>('lastModified');
  const [sortDirection, setSortDirection] = useState<'asc' | 'desc'>('desc');
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(10);
  const width = useElementWidth(rootRef);
  const columns = useMemo(() => visibleArtifactColumns(width), [width]);
  const gridTemplateColumns = useMemo(() => artifactGridTemplate(columns), [columns]);
  const breadcrumbs = useMemo(() => parsePrefixToBreadcrumbs(props.currentPrefix), [props.currentPrefix]);
  const levelItems = useMemo(
    () => getItemsAtCurrentLevel(props.contents, props.currentPrefix),
    [props.contents, props.currentPrefix],
  );
  const visibleItems = useMemo(() => {
    const needle = query.trim().toLowerCase();
    const matching = levelItems.filter((item) => needle === '' || item.name.toLowerCase().includes(needle));
    return sortItems(matching, sortField, sortDirection);
  }, [levelItems, query, sortDirection, sortField]);
  const paginated = useMemo(
    () => visibleItems.slice(page * pageSize, page * pageSize + pageSize),
    [page, pageSize, visibleItems],
  );
  const selectableIds = useMemo(() => visibleItems.map((item) => item.id), [visibleItems]);
  const selectedItems = useMemo(
    () => visibleItems.filter((item) => selected.has(item.id)),
    [selected, visibleItems],
  );
  const allSelected = selectableIds.length > 0 && selectableIds.every((id) => selected.has(id));
  const someSelected = selectableIds.some((id) => selected.has(id));

  useEffect(() => {
    setSelected((current) => new Set([...current].filter((id) => selectableIds.includes(id))));
    if (page * pageSize >= visibleItems.length) setPage(0);
  }, [page, pageSize, selectableIds, visibleItems.length]);

  const toggleSort = (field: ArtifactSortField): void => {
    if (sortField === field) setSortDirection((current) => (current === 'asc' ? 'desc' : 'asc'));
    else {
      setSortField(field);
      setSortDirection('asc');
    }
  };
  const stageInputFiles = (event: ChangeEvent<HTMLInputElement>): void => {
    const files = event.target.files;
    if (files !== null) props.onUpload([...files]);
    event.target.value = '';
  };
  const handleDrop = (event: DragEvent<HTMLDivElement>): void => {
    event.preventDefault();
    props.onUpload([...event.dataTransfer.files]);
  };
  const toggleRow = (id: string): void => {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <Box
      ref={rootRef}
      sx={rootSx}
      onDragOver={(event) => event.preventDefault()}
      onDrop={handleDrop}
    >
      <ArtifactTableToolbar
        bucket={props.bucket}
        breadcrumbs={breadcrumbs}
        fileCount={levelItems.length}
        retentionDays={props.retentionDays}
        query={query}
        hasSelection={someSelected}
        fileInputRef={inputRef}
        onQueryChange={(value) => {
          setQuery(value);
          setPage(0);
        }}
        onBreadcrumbClick={props.onPrefixChange}
        onFilesPicked={stageInputFiles}
        onDownloadSelected={() => props.onDownloadMany(selectedItems)}
        onDeleteSelected={() => props.onDelete(selectedItems)}
      />
      {props.error !== undefined && (
        <Typography
          role="alert"
          sx={messageSx}
        >
          {props.error}
        </Typography>
      )}
      {props.loading ? (
        <Typography sx={messageSx}>{t('artifacts.table.loading', 'Loading files…')}</Typography>
      ) : visibleItems.length === 0 ? (
        <ArtifactTableEmpty onUpload={() => inputRef.current?.click()} />
      ) : (
        <>
          <Box
            component="table"
            sx={tableSx}
          >
            <Box
              component="thead"
              sx={headSx}
            >
              <ArtifactGridHeader
                columns={columns}
                gridTemplateColumns={gridTemplateColumns}
                sortField={sortField}
                sortDirection={sortDirection}
                allSelected={allSelected}
                someSelected={someSelected}
                onSelectAll={() => setSelected(allSelected ? new Set() : new Set(selectableIds))}
                onSort={toggleSort}
              />
            </Box>
            <Box
              component="tbody"
              sx={bodySx}
            >
              {paginated.map((item) => (
                <ArtifactGridRow
                  key={item.id}
                  item={item}
                  columns={columns}
                  gridTemplateColumns={gridTemplateColumns}
                  values={cellValues(item)}
                  selected={selected.has(item.id)}
                  onToggle={toggleRow}
                  onOpen={() => props.onPrefixChange(item.key)}
                  onPreview={props.onPreview}
                  onDownload={props.onDownload}
                  onDelete={(target) => props.onDelete([target])}
                />
              ))}
            </Box>
          </Box>
          <ArtifactPagination
            total={visibleItems.length}
            page={page}
            pageSize={pageSize}
            onPageChange={setPage}
            onPageSizeChange={(size) => {
              setPageSize(size);
              setPage(0);
            }}
          />
        </>
      )}
    </Box>
  );
}

const rootSx: SxProps<Theme> = (theme) => ({
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
  overflow: 'hidden',
  gap: theme.spacing(0.375),
});
const tableSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  minHeight: 0,
  gap: theme.spacing(1.5),
  overflow: 'hidden',
  paddingInline: theme.spacing(3),
});
const headSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', flexShrink: 0 };
const bodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  overflow: 'auto',
};
const messageSx: SxProps<Theme> = (theme) => ({ padding: theme.spacing(2, 3) });
