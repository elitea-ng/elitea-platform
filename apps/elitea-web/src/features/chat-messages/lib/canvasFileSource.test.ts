import { describe, expect, it } from 'vitest';

import {
  CANVAS_FILE_OPEN_SIZE_LIMIT_BYTES,
  detectCanvasFileOpenKind,
  isCanvasFileOpenSizeOk,
  isUnsupportedCanvasDocumentFormat,
} from './canvasFileSource';

describe('detectCanvasFileOpenKind', () => {
  it('maps a mermaid file to a diagram', () => {
    expect(detectCanvasFileOpenKind('flow.mmd')).toEqual({ type: 'diagram', language: 'mermaid' });
    expect(detectCanvasFileOpenKind('FLOW.MERMAID')).toEqual({ type: 'diagram', language: 'mermaid' });
  });

  it('maps csv/tsv to a table', () => {
    expect(detectCanvasFileOpenKind('report.csv')).toEqual({ type: 'table', language: 'markdownTable' });
    expect(detectCanvasFileOpenKind('report.tsv')).toEqual({ type: 'table', language: 'markdownTable' });
  });

  it('maps a code file to code, with its language', () => {
    expect(detectCanvasFileOpenKind('script.py')).toEqual({ type: 'code', language: 'python' });
    expect(detectCanvasFileOpenKind('module.ts')).toEqual({ type: 'code', language: 'typescript' });
  });

  it('maps markdown/text to a document (issue #879) — prose, not a single table or a code fence', () => {
    expect(detectCanvasFileOpenKind('README.md')).toEqual({ type: 'document', language: 'document' });
    expect(detectCanvasFileOpenKind('notes.markdown')).toEqual({ type: 'document', language: 'document' });
    expect(detectCanvasFileOpenKind('notes.txt')).toEqual({ type: 'document', language: 'document' });
  });

  it('is nested-path and case aware', () => {
    expect(detectCanvasFileOpenKind('folder/nested/Notes.TXT')).toEqual({ type: 'document', language: 'document' });
  });

  it('refuses a binary/office kind and an unrecognised extension', () => {
    expect(detectCanvasFileOpenKind('photo.png')).toBeUndefined();
    expect(detectCanvasFileOpenKind('report.docx')).toBeUndefined();
    expect(detectCanvasFileOpenKind('archive.zip')).toBeUndefined();
    expect(detectCanvasFileOpenKind('no-extension-at-all')).toBeUndefined();
  });
});

describe('isUnsupportedCanvasDocumentFormat', () => {
  it('names .docx and its office siblings as recognised-but-unopenable (issue #879)', () => {
    expect(isUnsupportedCanvasDocumentFormat('report.docx')).toBe(true);
    expect(isUnsupportedCanvasDocumentFormat('Report.DOCX')).toBe(true);
    expect(isUnsupportedCanvasDocumentFormat('sheet.xlsx')).toBe(true);
  });

  it('is false for an openable kind and for an extension it has never heard of', () => {
    expect(isUnsupportedCanvasDocumentFormat('README.md')).toBe(false);
    expect(isUnsupportedCanvasDocumentFormat('archive.zip')).toBe(false);
  });
});

describe('isCanvasFileOpenSizeOk', () => {
  it('allows an unstated size (nothing to cap against)', () => {
    expect(isCanvasFileOpenSizeOk(undefined)).toBe(true);
  });

  it('allows a file at or under the cap and refuses one over it', () => {
    expect(isCanvasFileOpenSizeOk(CANVAS_FILE_OPEN_SIZE_LIMIT_BYTES)).toBe(true);
    expect(isCanvasFileOpenSizeOk(CANVAS_FILE_OPEN_SIZE_LIMIT_BYTES + 1)).toBe(false);
  });
});

/**
 * Issues programme, package C-chat — three CLOSED legacy canvas-file-type
 * issues, judged against this pure function (no live conversation needed).
 */
describe('canvas file-type support (elitea_issues #5051, #5846, #5940, #5845)', () => {
  /* elitea_issues: #5051 — NOT REPRODUCED: an .xml file already opens in canvas as
   * syntax-highlighted code (present in both CODE_LANGUAGE_BY_EXTENSION and
   * OPENABLE_TEXT_EXTENSIONS, and in canvasLanguageOptions.ts's language picker). */
  it('#5051: an .xml file is openable in canvas (code kind, xml language)', () => {
    expect(detectCanvasFileOpenKind('notes.xml')).toEqual({ type: 'code', language: 'xml' });
  });

  /* elitea_issues: #5846, #5940 — product gap: .mdx is not a member of
   * CODE_LANGUAGE_BY_EXTENSION, DOCUMENT_EXTENSIONS, or OPENABLE_TEXT_EXTENSIONS at
   * all, so it falls through every branch and canvas refuses it as unsupported —
   * no Preview/Raw mode, and (same root cause) no thumbnail/Edit/Preview affordance
   * for an mdx file returned in a chat result. */
  it.fails('#5846/#5940: an .mdx file should be recognised by canvas (Preview/Raw, like markdown)', () => {
    expect(detectCanvasFileOpenKind('report.mdx')).toBeDefined();
  });

  /* elitea_issues: #5845 — product gap: .html maps only to a plain `code` kind; there
   * is no rendered-HTML "Preview" mode distinct from "Raw" the way markdown/prose
   * files get the `document` kind. */
  it.fails('#5845: an .html file should offer a rendered Preview mode, not only raw code', () => {
    expect(detectCanvasFileOpenKind('report.html')?.type).toBe('document');
  });
});
