import { describe, expect, it } from 'vitest';

import {
  MAX_ATTACHMENTS,
  MAX_ATTACHMENT_FILE_BYTES,
  validateWikiAttachmentFiles,
} from './attachmentValidation';

function textFile(name: string, sizeBytes = 10): File {
  const file = new File([new Uint8Array(sizeBytes)], name, { type: 'text/plain' });
  return file;
}

describe('validateWikiAttachmentFiles', () => {
  it('accepts a small, allow-listed text file', () => {
    const result = validateWikiAttachmentFiles([textFile('notes.md')], []);
    expect(result.validFiles).toHaveLength(1);
    expect(result.errors).toEqual([]);
  });

  it('rejects a file whose extension is not on the allowlist', () => {
    const result = validateWikiAttachmentFiles([textFile('photo.png')], []);
    expect(result.validFiles).toHaveLength(0);
    expect(result.errors[0]).toContain('photo.png');
    expect(result.errors[0]).toContain('not a supported text file type');
  });

  it('rejects a file over the per-file size cap', () => {
    const result = validateWikiAttachmentFiles(
      [textFile('big.txt', MAX_ATTACHMENT_FILE_BYTES + 1)],
      [],
    );
    expect(result.validFiles).toHaveLength(0);
    expect(result.errors[0]).toContain('big.txt');
  });

  it('accepts a file exactly at the size cap', () => {
    const result = validateWikiAttachmentFiles([textFile('at-cap.txt', MAX_ATTACHMENT_FILE_BYTES)], []);
    expect(result.validFiles).toHaveLength(1);
  });

  // MAX_ATTACHMENTS matches the engine's own MaxExtraContextFiles — see the
  // module doc comment for why a 6th slot should never be offered at all.
  it('caps the total at MAX_ATTACHMENTS, counting what is already attached', () => {
    const existing = Array.from({ length: MAX_ATTACHMENTS - 1 }, (_, i) => ({ name: `f${String(i)}.txt` }));
    const result = validateWikiAttachmentFiles([textFile('a.txt'), textFile('b.txt')], existing);
    expect(result.validFiles).toHaveLength(1);
    expect(result.errors.some((e) => e.includes('at most'))).toBe(true);
  });

  it('refuses everything once the cap is already reached', () => {
    const existing = Array.from({ length: MAX_ATTACHMENTS }, (_, i) => ({ name: `f${String(i)}.txt` }));
    const result = validateWikiAttachmentFiles([textFile('a.txt')], existing);
    expect(result.validFiles).toHaveLength(0);
    expect(result.errors).toHaveLength(1);
  });

  it('reports one file’s failure without dropping a valid sibling', () => {
    const result = validateWikiAttachmentFiles([textFile('good.md'), textFile('bad.exe')], []);
    expect(result.validFiles.map((f) => f.name)).toEqual(['good.md']);
    expect(result.errors).toHaveLength(1);
  });
});
