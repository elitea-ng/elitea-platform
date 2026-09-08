/**
 * Taking a table canvas out of the app as a file — CSV and XLSX.
 *
 * ── WHY THIS IS HAND-BUILT AND NOT A LIBRARY ──────────────────────────────
 * The reference's export footer ran on `excellentexport`, which reads a
 * RENDERED `<table>` out of the DOM and hands it to the browser. Neither that
 * package nor any other spreadsheet writer is a dependency here, and adding
 * one is a decision with a gate attached (`scripts/check-budgets.mjs` for the
 * source budgets, `scripts/check-bundle-budget.mjs` for what ships). It is
 * also not needed: `jszip` is ALREADY a direct dependency of this app (the
 * artifact download path uses it), and an `.xlsx` file is a zip of XML parts.
 * So the workbook is written here, from the table's own model rather than from
 * a DOM node — which is the better source anyway, since a virtualised grid
 * does not have every row in the DOM to read.
 *
 * The workbook is deliberately the SMALLEST one Excel, Numbers and LibreOffice
 * all open: five parts, one sheet, every cell an inline string. No shared
 * string table (its only benefit is size on a table with heavy repetition),
 * no styles part (a canvas table carries no formatting), no calc chain (there
 * are no formulas). Every cell is written as `t="inlineStr"` — including one
 * that looks like a number, because the table model holds text and a writer
 * that guessed at types would silently reformat a part number or a version
 * string.
 *
 * ── WHAT IS NOT DONE, STATED ──────────────────────────────────────────────
 * A cell whose text begins `=`, `+`, `-` or `@` is written verbatim, and a
 * spreadsheet may treat it as a formula. It is left verbatim on purpose: the
 * app's own CSV/TSV import reads these files back, and a client-side escape
 * would make the export stop round-tripping through it. The same is true of
 * the reference's exporter.
 */
import JSZip from 'jszip';

import { triggerBlobDownload } from '@/shared/lib/download';

import type { MarkdownTableData } from './markdownTable';

/** The two formats the export control offers. */
export type TableExportFormat = 'csv' | 'xlsx';

/** One CSV field, quoted only when it has to be (RFC 4180). */
function csvField(value: string): string {
  return /[",\r\n]/.test(value) ? `"${value.replaceAll('"', '""')}"` : value;
}

/** The whole table as CSV, header row first, CRLF line endings (RFC 4180). */
export function tableToCsv({ headers, rows }: MarkdownTableData): string {
  const lines = [headers.map(csvField).join(',')];
  for (const row of rows) lines.push(headers.map((_, index) => csvField(row[index] ?? '')).join(','));
  return lines.join('\r\n');
}

/** `0 → A`, `25 → Z`, `26 → AA` — a spreadsheet column name. */
export function columnName(index: number): string {
  let name = '';
  let n = index;
  while (n >= 0) {
    name = String.fromCharCode(65 + (n % 26)) + name;
    n = Math.floor(n / 26) - 1;
  }
  return name;
}

/** XML text escaping. `&` first, or it would escape the escapes. */
function xml(value: string): string {
  return value
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&apos;');
}

function sheetRow(cells: readonly string[], rowNumber: number): string {
  const body = cells
    .map(
      (cell, column) =>
        `<c r="${columnName(column)}${String(rowNumber)}" t="inlineStr"><is><t xml:space="preserve">${xml(cell)}</t></is></c>`,
    )
    .join('');
  return `<row r="${String(rowNumber)}">${body}</row>`;
}

/** The one part of the workbook that carries data — exported so the writer can be read without unzipping. */
export function buildSheetXml({ headers, rows }: MarkdownTableData): string {
  const body = [
    sheetRow(headers, 1),
    ...rows.map((row, index) => sheetRow(headers.map((_, column) => row[column] ?? ''), index + 2)),
  ].join('');
  return (
    `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
    `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>${body}</sheetData></worksheet>`
  );
}

const CONTENT_TYPES =
  `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
  `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
  `<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
  `<Default Extension="xml" ContentType="application/xml"/>` +
  `<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
  `<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
  `</Types>`;

const ROOT_RELS =
  `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
  `<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
  `</Relationships>`;

const WORKBOOK =
  `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
  `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
  `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
  `<sheets><sheet name="Table" sheetId="1" r:id="rId1"/></sheets></workbook>`;

const WORKBOOK_RELS =
  `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
  `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
  `<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
  `</Relationships>`;

export const XLSX_MIME = 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet';

/** The whole `.xlsx`, as a blob ready to save. */
export async function tableToXlsxBlob(data: MarkdownTableData): Promise<Blob> {
  const zip = new JSZip();
  zip.file('[Content_Types].xml', CONTENT_TYPES);
  zip.file('_rels/.rels', ROOT_RELS);
  zip.file('xl/workbook.xml', WORKBOOK);
  zip.file('xl/_rels/workbook.xml.rels', WORKBOOK_RELS);
  zip.file('xl/worksheets/sheet1.xml', buildSheetXml(data));
  return zip.generateAsync({ type: 'blob', mimeType: XLSX_MIME });
}

/**
 * Saves the table under `filename`.`format`.
 *
 * The file leaves through `shared/lib/download`'s `triggerBlobDownload`, the
 * one sanctioned blob→anchor primitive in this app, for the reason that module
 * documents: a `window.location` navigation can raise the SPA's own
 * `beforeunload` prompt, and the detached-anchor click never navigates.
 * The conversation export takes the same path.
 */
export async function exportTable(data: MarkdownTableData, format: TableExportFormat, filename = 'table'): Promise<void> {
  const blob =
    format === 'csv'
      ? new Blob([tableToCsv(data)], { type: 'text/csv;charset=utf-8' })
      : await tableToXlsxBlob(data);
  triggerBlobDownload(blob, `${filename}.${format}`);
}
