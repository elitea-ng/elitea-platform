/**
 * The two files a table canvas can leave as.
 *
 * The XLSX case unzips what the writer produced and reads the cell back,
 * because "a blob was returned" is exactly the assertion that would stay green
 * for a corrupt workbook — and a workbook Excel refuses to open is a download
 * the reader discovers is broken only after the app has told them it worked.
 */
import JSZip from 'jszip';
import { describe, expect, it, vi } from 'vitest';

import { XLSX_MIME, buildSheetXml, columnName, exportTable, tableToCsv, tableToXlsxBlob } from './tableExport';

const TABLE = {
  headers: ['Metric', 'Value, with comma'],
  rows: [
    ['alpha', 'a "quoted" cell'],
    ['日本語', 'line1\nline2'],
  ],
} as const;

describe('tableToCsv', () => {
  it('quotes only the fields that need it, and doubles an embedded quote', () => {
    expect(tableToCsv(TABLE)).toBe(
      ['Metric,"Value, with comma"', 'alpha,"a ""quoted"" cell"', '日本語,"line1\nline2"'].join('\r\n'),
    );
  });

  it('pads a short row rather than shifting the columns left', () => {
    expect(tableToCsv({ headers: ['a', 'b', 'c'], rows: [['1']] })).toBe('a,b,c\r\n1,,');
  });
});

describe('columnName', () => {
  it('runs past Z the way a spreadsheet does', () => {
    expect([0, 1, 25, 26, 27, 51, 52].map(columnName)).toEqual(['A', 'B', 'Z', 'AA', 'AB', 'AZ', 'BA']);
  });
});

describe('buildSheetXml', () => {
  it('escapes the five XML characters instead of writing a broken part', () => {
    const xml = buildSheetXml({ headers: ['a & b'], rows: [['<tag>']] });
    expect(xml).toContain('a &amp; b');
    expect(xml).toContain('&lt;tag&gt;');
    expect(xml).not.toContain('<tag>');
  });

  it('writes every cell as an inline string, so a version number stays text', () => {
    const xml = buildSheetXml({ headers: ['v'], rows: [['1.10']] });
    expect(xml).toContain('t="inlineStr"');
    expect(xml).toContain('<t xml:space="preserve">1.10</t>');
  });
});

describe('tableToXlsxBlob', () => {
  it('produces a zip carrying the five parts a reader needs, with the data in the sheet', async () => {
    const blob = await tableToXlsxBlob(TABLE);
    const zip = await JSZip.loadAsync(await blob.arrayBuffer());
    for (const part of ['[Content_Types].xml', '_rels/.rels', 'xl/workbook.xml', 'xl/_rels/workbook.xml.rels', 'xl/worksheets/sheet1.xml']) {
      expect(zip.file(part), `the workbook must carry ${part}`).not.toBeNull();
    }
    const sheet = await zip.file('xl/worksheets/sheet1.xml')?.async('string');
    expect(sheet).toContain('<c r="A1" t="inlineStr"><is><t xml:space="preserve">Metric</t></is></c>');
    expect(sheet).toContain('日本語');
    expect(blob.type).toBe(XLSX_MIME);
  });
});

describe('exportTable', () => {
  it('hands the file to the shared download primitive, named by the format', async () => {
    const clicks: string[] = [];
    const createObjectURL = vi.fn(() => 'blob:test');
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function mockClick(this: HTMLAnchorElement) {
      clicks.push(this.download);
    });

    try {
      await exportTable(TABLE, 'csv', 'autotest');
      await exportTable(TABLE, 'xlsx', 'autotest');
      expect(clicks).toEqual(['autotest.csv', 'autotest.xlsx']);
      expect(createObjectURL).toHaveBeenCalledTimes(2);
      // The object URL is released each time — a download that leaks one holds
      // the whole blob in memory for the life of the tab.
      expect(revokeObjectURL).toHaveBeenCalledTimes(2);
    } finally {
      click.mockRestore();
    }
  });
});
