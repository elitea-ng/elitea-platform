import { describe, expect, it } from 'vitest';
import { appendToolOutputChunk } from './toolOutputChunks';

const hash = 'a'.repeat(64);
describe('tool output chunks', () => {
  it('uses UTF-8 offsets and accepts exact replay without duplication', () => {
    const first = { offset_bytes: 0, total_bytes: 5, sha256: hash, final: false };
    const part = appendToolOutputChunk(undefined, '界', first, undefined);
    expect(part?.output).toBe('界');
    expect(appendToolOutputChunk(part?.output, '界', first, part?.chunk)).toEqual(part);
    const end = { offset_bytes: 3, total_bytes: 5, sha256: hash, final: true };
    const result = appendToolOutputChunk(part?.output, 'ab', end, part?.chunk);
    expect(result?.output).toBe('界ab');
    expect(result?.chunk.final).toBe(true);
    expect(appendToolOutputChunk(result?.output, '界', first, result?.chunk)).toEqual(result);
  });
  it('refuses missing and conflicting chunks', () => {
    const first = { offset_bytes: 0, total_bytes: 3, sha256: hash, final: false };
    const part = appendToolOutputChunk(undefined, 'a', first, undefined);
    expect(appendToolOutputChunk(part?.output, 'x', first, part?.chunk)).toBeUndefined();
    expect(appendToolOutputChunk(part?.output, 'c', { ...first, offset_bytes: 2, final: true }, part?.chunk)).toBeUndefined();
    expect(appendToolOutputChunk(part?.output, 'bc', { ...first, offset_bytes: 1, final: true, sha256: 'b'.repeat(64) }, part?.chunk)).toBeUndefined();
  });
});
