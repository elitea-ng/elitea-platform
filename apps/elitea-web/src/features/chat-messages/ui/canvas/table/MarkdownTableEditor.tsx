/**
 * ui/canvas/table/MarkdownTableEditor.tsx — the canvas markdown-table editor
 * (Canvas slice 3), ported from `apps/elitea-ui/src/components/
 * MarkdownTableEditor.jsx` (859 lines).
 *
 * THE SPLIT (§3.5 caps a file at 400 lines and a component at 12 props):
 *   - `../../../lib/markdownTable.ts`     — parse/serialise/CSV, pure
 *   - `../../../model/useMarkdownTableModel.ts` — columns/rows/history/selection
 *   - `./markdownTableCells.tsx`          — the 3 DataGrid renderers
 *   - this file                           — the grid itself + the ref API
 * The 10 baseline props are grouped into 4 here (`content`, `history`,
 * `tracking`, plus `readOnly`), the same grouping `CanvasEditHeader` already
 * uses in this slice.
 *
 * **DEVIATIONS (disclosed):**
 *  1. The download lives in the HEADER, not in a footer. The baseline
 *     rendered `useDownloadTable` + `SplitButton` under the grid, and the
 *     exporter read a hidden `Markdown tableId=…` DOM node — which is why
 *     `tableId` existed at all. Both files are written from the table MODEL
 *     now (`../../../lib/tableExport`), so there is no hidden node and no DOM
 *     scrape, and the control sits with the grid's other actions in
 *     `../CanvasEditHeader`. `tracking.interaction_uuid`/`conversation_uuid`
 *     are still carried and still unread — they belonged to the baseline's GA
 *     telemetry, which this app has none of.
 *  2. Delete asks for confirmation through `shared/ui`'s `DeleteEntityModal`
 *     rather than the baseline's own `AlertDialog` component.
 *  3. `GRID_CHECKBOX_SELECTION_COL_DEF` is not spread into the column list.
 *     `@mui/x-data-grid` v9 adds that column itself from `checkboxSelection`,
 *     and its selection model is `{ type, ids: Set }`, not the v7 id array
 *     the baseline destructured.
 */
import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useState } from 'react';

import { Box } from '@mui/material';
import { DataGrid, type GridColDef, type GridRowSelectionModel } from '@mui/x-data-grid';

import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { t } from '@/shared/i18n';

import type { MarkdownTableData } from '../../../lib/markdownTable';
import { parseMarkdownTable } from '../../../lib/markdownTable';
import type { MarkdownTableRow, MarkdownTableSelection } from '../../../model/useMarkdownTableModel';
import { useMarkdownTableModel } from '../../../model/useMarkdownTableModel';
import { CellEditor, ColumnHeader, ExpandableCell } from './markdownTableCells';

/** The imperative surface `CanvasEditor` drives through its editor ref. */
export interface MarkdownTableEditorHandle {
  readonly undo: () => void;
  readonly redo: () => void;
  readonly addRow: () => void;
  readonly addColumn: () => void;
  /** Opens the delete confirmation for the current row/column selection. */
  readonly delete: () => void;
  /** Replaces the whole table — canvas sync and CSV/TSV import both land here. */
  readonly resetTable: (data: MarkdownTableData) => void;
  /** Replaces the whole table from raw markdown (canvas sync). */
  readonly setCode: (markdown: string) => void;
  readonly getCode: () => string;
}

export interface MarkdownTableEditorProps {
  readonly content: {
    readonly initialMarkdown: string;
    readonly onChange?: ((markdown: string) => void) | undefined;
  };
  readonly history?: {
    readonly onCanUndo?: ((canUndo: boolean) => void) | undefined;
    readonly onCanRedo?: ((canRedo: boolean) => void) | undefined;
  };
  readonly onRowsColumnsSelected?: ((selection: MarkdownTableSelection) => void) | undefined;
  readonly readOnly?: boolean | undefined;
  // Deviation 1: carried for the baseline's GA telemetry, which this app does
  // not have; nothing reads them today.
  readonly tracking?: {
    readonly interaction_uuid?: string | undefined;
    readonly conversation_uuid?: string | undefined;
  };
}

const CHANGE_DEBOUNCE_MS = 30;

/** Renders the markdown table as an editable grid and reports every edit back as markdown. */
export const MarkdownTableEditor = forwardRef<MarkdownTableEditorHandle, MarkdownTableEditorProps>(
  function MarkdownTableEditor({ content, history, onRowsColumnsSelected, readOnly }, ref) {
    const initialMarkdownData = useMemo(
      () => parseMarkdownTable(content.initialMarkdown),
      // Only the FIRST markdown matters: later document changes arrive through
      // `setCode`/`resetTable` on the ref, which is what keeps the grid's own
      // undo history alive across a canvas sync.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      [],
    );

    const model = useMarkdownTableModel({
      initialMarkdownData,
      onCanUndo: history?.onCanUndo,
      onCanRedo: history?.onCanRedo,
    });

    const [confirmDelete, setConfirmDelete] = useState(false);
    const [sortModel, setSortModel] = useState<{ field: string; sort: 'asc' | 'desc' } | null>(null);

    const { getCode, resetTable, selection } = model;
    const { onChange } = content;

    useImperativeHandle(
      ref,
      () => ({
        undo: model.undo,
        redo: model.redo,
        addRow: model.addRow,
        addColumn: model.addColumn,
        delete: () => {
          if (selection.hasSelectedRows || selection.hasSelectedColumns) setConfirmDelete(true);
        },
        resetTable,
        setCode: (markdown: string) => resetTable(parseMarkdownTable(markdown)),
        getCode,
      }),
      [getCode, model.addColumn, model.addRow, model.redo, model.undo, resetTable, selection],
    );

    // Debounced write-back, matching the baseline's 30ms `setTimeout`.
    useEffect(() => {
      const timer = setTimeout(() => onChange?.(getCode()), CHANGE_DEBOUNCE_MS);
      return () => clearTimeout(timer);
    }, [getCode, onChange]);

    useEffect(() => {
      onRowsColumnsSelected?.(selection);
    }, [onRowsColumnsSelected, selection]);

    const onRowSelectionModelChange = useCallback(
      (next: GridRowSelectionModel) => {
        model.setSelectedRowIds([...next.ids].map(Number));
      },
      [model],
    );

    const rowSelectionModel = useMemo<GridRowSelectionModel>(
      () => ({ type: 'include', ids: new Set(model.selectedRowIds) }),
      [model.selectedRowIds],
    );

    const columns = useMemo<GridColDef[]>(
      () =>
        model.columns.map((column): GridColDef => ({
          field: column.field,
          headerName: column.headerName,
          editable: readOnly !== true,
          sortable: false,
          minWidth: 160,
          flex: 1,
          ...(model.selectedColumns.includes(column.field)
            ? { headerClassName: 'MuiDataGrid-columnHeader--selected' }
            : {}),
          renderCell: (params) => <ExpandableCell {...params} />,
          renderEditCell: (params) => (
            <CellEditor
              {...params}
              readOnly={readOnly}
            />
          ),
          renderHeader: (params) => (
            <ColumnHeader
              {...params}
              rows={model.rows}
              onReorderRows={model.reorderRows}
              onRename={model.renameColumn}
              sortDirection={sortModel?.field === column.field ? sortModel.sort : undefined}
              onSort={(field, sort) => setSortModel({ field, sort })}
              readOnly={readOnly}
            />
          ),
        })),
      [model, readOnly, sortModel],
    );

    return (
      <>
        <Box
          data-testid="chat-table-canvas-grid"
          sx={{ display: 'flex', flexDirection: 'column', height: '100%', width: '100%', overflow: 'hidden' }}
        >
          <DataGrid
            rows={model.rows as MarkdownTableRow[]}
            columns={columns}
            getRowHeight={() => 'auto'}
            getEstimatedRowHeight={() => 200}
            processRowUpdate={(updated: MarkdownTableRow) => {
              model.updateRow(updated);
              return updated;
            }}
            showCellVerticalBorder
            showColumnVerticalBorder
            disableColumnSorting
            disableColumnMenu
            pageSizeOptions={[5, 10, 50, 100]}
            initialState={{ pagination: { paginationModel: { pageSize: 50, page: 0 } } }}
            onCellClick={(params) => model.setSelectedCell({ rowId: Number(params.id), columnField: params.field })}
            onColumnHeaderClick={(params) => {
              model.setSelectedCell({ columnField: params.field });
              model.toggleColumnSelection(params.field);
            }}
            getCellClassName={(params) =>
              model.selectedColumns.includes(params.field) ? 'MuiDataGrid-cell--selected' : ''
            }
            checkboxSelection={readOnly !== true && model.rows.length > 0}
            disableRowSelectionOnClick
            rowSelectionModel={rowSelectionModel}
            onRowSelectionModelChange={onRowSelectionModelChange}
          />
        </Box>
        <DeleteEntityModal
          open={confirmDelete}
          onClose={() => setConfirmDelete(false)}
          onConfirm={() => {
            model.deleteSelection();
            setConfirmDelete(false);
          }}
          copy={{ title: t('features.chatMessages.canvas.table.deleteTitle', 'Warning') }}
          content={{
            custom: selection.hasSelectedRows
              ? t('features.chatMessages.canvas.table.deleteRows', 'Are you sure to delete the selected rows?')
              : t('features.chatMessages.canvas.table.deleteColumns', 'Are you sure to delete the selected columns?'),
          }}
        />
      </>
    );
  },
);

MarkdownTableEditor.displayName = 'MarkdownTableEditor';
