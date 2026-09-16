/**
 * The allow-list derivation (#940 A15). Pure, so every rule is asserted
 * without a query client — the hook over it is four lines.
 */
import { describe, expect, it } from 'vitest';

import {
  deriveAllowedAttachmentTypes,
  fileExtension,
  normaliseExtension,
} from './allowedTypes';

describe('normaliseExtension', () => {
  it('lower-cases and guarantees the leading dot', () => {
    expect(normaliseExtension('PDF')).toBe('.pdf');
    expect(normaliseExtension('.PDF')).toBe('.pdf');
    expect(normaliseExtension('  .Md ')).toBe('.md');
  });

  it('answers an empty string for an empty input, not a bare dot', () => {
    // A bare '.' in the allow-list would match `fileExtension('a.')`, which is
    // the one shape that must never be accepted.
    expect(normaliseExtension('')).toBe('');
    expect(normaliseExtension('   ')).toBe('');
  });
});

describe('fileExtension', () => {
  it('reads the LAST extension', () => {
    expect(fileExtension('archive.tar.gz')).toBe('.gz');
  });

  it('answers empty for a dotfile, a trailing dot, and a name with no dot', () => {
    // `.env` is a NAME, not an extension — the dot is at index 0.
    expect(fileExtension('.env')).toBe('');
    expect(fileExtension('report.')).toBe('');
    expect(fileExtension('Makefile')).toBe('');
  });
});

describe('deriveAllowedAttachmentTypes', () => {
  const served = {
    items: [],
    total: 0,
    document_types: { '.pdf': 'application/pdf', '.TXT': 'text/plain' },
    image_types: { '.png': 'image/png' },
    code_types: { '.sql': 'text/x-sql', '.sh': 'text/x-shellscript' },
  };

  it('folds all three categories into one normalised list', () => {
    const result = deriveAllowedAttachmentTypes(served);
    expect(result.extensions).toEqual(['.pdf', '.png', '.sh', '.sql', '.svg', '.txt']);
    expect(result.mimeTypes).toContain('application/pdf');
    expect(result.isServed).toBe(true);
  });

  it('appends .svg to a populated list, the way the baseline does', () => {
    // `fileTypes.js:30` appends it after the three served categories: the
    // indexer does not load SVG but the composer accepts it, and
    // `shared/lib/attachments.ts` already gives it the non-image size cap.
    expect(deriveAllowedAttachmentTypes(served).extensions).toContain('.svg');
  });

  it('leaves an EMPTY served answer empty — including of .svg', () => {
    // Appending SVG here would turn "nothing is allowed" into "one thing is",
    // and hide the state ELITEA-0489 is about.
    const result = deriveAllowedAttachmentTypes({ items: [], total: 0 });
    expect(result.extensions).toEqual([]);
    expect(result.isServed).toBe(true);
  });

  it('reports isServed false for an ABSENT answer, which is a different thing', () => {
    const result = deriveAllowedAttachmentTypes(undefined);
    expect(result.extensions).toEqual([]);
    expect(result.isServed).toBe(false);
  });
});
