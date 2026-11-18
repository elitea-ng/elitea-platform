import { useMemo } from 'react';

import { useSearch } from '@tanstack/react-router';

import { isPinnedGroupId, useEntityFolders, type EntityFolderKind } from '@/entities/entity-folder';
import { t } from '@/shared/i18n';

/** The `sort_by` value of a folder's own order. */
const FOLDER_ORDER_SORT = 'folder';

interface SortField {
  readonly value: string;
  readonly label: string;
  readonly directionless?: boolean;
}

interface SortFields {
  readonly items: readonly SortField[];
  readonly defaultField: string;
}

/** The open folder and its displayed order, for the drag and drop that rearranges it (`EntityFolderDnd`'s `folderOrder`). */
export interface FolderOrder {
  readonly folderId: string;
  /** The entity ids on screen, top to bottom. */
  readonly ids: readonly number[];
}

interface FolderOrderInput<TRow> {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  /** The open folder (`useFolderFilter().folderId`); the Pinned group has no order of its own. */
  readonly folderId: string | undefined;
  /** The page's own sort fields; "Folder order" joins them while a folder is open. */
  readonly sortFields: SortFields;
  /** The rows as the server sorted them. */
  readonly rows: readonly TRow[];
  readonly idOf: (row: TRow) => number;
}

interface FolderOrderResult<TRow> {
  /** The sort fields for the sidebar: the page's, plus "Folder order" (the default) inside a folder. */
  readonly sortFields: SortFields;
  /** The rows in the order to draw them. */
  readonly rows: readonly TRow[];
  /** Set while the list shows the folder's own order, so cards can be dragged to a new place; undefined otherwise. */
  readonly folderOrder: FolderOrder | undefined;
}

/** `rows` in the order of `itemIds`; a row the folder does not list yet (a refetch in flight) keeps its place after them. */
function inFolderOrder<TRow>(rows: readonly TRow[], itemIds: readonly number[], idOf: (row: TRow) => number): TRow[] {
  const rank = new Map(itemIds.map((id, index) => [id, index]));
  const last = itemIds.length;
  return rows
    .map((row, index) => ({ row, index, rank: rank.get(idOf(row)) ?? last }))
    .sort((a, b) => a.rank - b.rank || a.index - b.index)
    .map((entry) => entry.row);
}

/**
 * The "Folder order" sort (issue 2079): inside an open folder the list can
 * show the order the project keeps for it (the server's
 * `project_folder_items.position`, which `item_ids` follows), and it is the
 * default there. The rows come from the server in the page's own sort and are
 * rearranged here, so the page fetches exactly as before.
 */
export function useFolderOrder<TRow>({ projectId, kind, folderId, sortFields, rows, idOf }: FolderOrderInput<TRow>): FolderOrderResult<TRow> {
  const search = useSearch({ strict: false }) as { readonly sort_by?: unknown };
  const folders = useEntityFolders(projectId, kind).data;
  const folder = folderId === undefined || isPinnedGroupId(folderId) ? undefined : folders?.find((candidate) => candidate.id === folderId);
  const itemIds = folder?.item_ids;
  const sortBy = search.sort_by;

  return useMemo(() => {
    if (itemIds === undefined || folder === undefined) return { sortFields, rows, folderOrder: undefined };
    const fields: SortFields = {
      items: [{ value: FOLDER_ORDER_SORT, label: t('widgets.entityFolders.order.sort', 'Folder order'), directionless: true }, ...sortFields.items],
      defaultField: FOLDER_ORDER_SORT,
    };
    // The sidebar falls back to the default for a value it does not offer: so does the list.
    const chosen = sortFields.items.some((item) => item.value === sortBy);
    if (chosen) return { sortFields: fields, rows, folderOrder: undefined };
    const ordered = inFolderOrder(rows, itemIds, idOf);
    return { sortFields: fields, rows: ordered, folderOrder: { folderId: folder.id, ids: ordered.map(idOf) } };
  }, [itemIds, folder, sortFields, rows, idOf, sortBy]);
}
