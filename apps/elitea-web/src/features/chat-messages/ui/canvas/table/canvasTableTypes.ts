import type { MarkdownTableData } from '../../../lib/markdownTable';
import type { TableExportFormat } from '../../../lib/tableExport';

/**
 * The table-editing control surface, shared by `CanvasEditHeader` (which renders
 * the cluster) and `CanvasTableControls` (which is the cluster).
 *
 * It lives HERE rather than on either of them because both need it: the header
 * imported the controls and the controls imported the header's type, which is a
 * cycle `no-circular` rejects. A type both sides depend on belongs to neither.
 */
export interface CanvasEditHeaderTable {
  readonly isTableEditing?: boolean | undefined;
  readonly hasSelectedRowsColumns?: {
    readonly hasSelectedRows: boolean;
    readonly hasSelectedColumns: boolean;
  } | undefined;
  readonly onClickAddColumn?: (() => void) | undefined;
  readonly onClickAddRow?: (() => void) | undefined;
  readonly onDeleteSelectedRowsOrColumns?: (() => void) | undefined;
  readonly onImportTableData?: ((data: MarkdownTableData) => void) | undefined;
  /** Surfaces a failed CSV/TSV read from the import picker (this app has no toast hook yet). */
  readonly onImportError?: ((error: unknown) => void) | undefined;
  /** Saves the table as a file. Omitted, no export control is rendered — the same rule every other control here follows. */
  readonly onExportTable?: ((format: TableExportFormat) => void) | undefined;
}
