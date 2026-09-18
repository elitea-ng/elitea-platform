import { describe, expect, it } from 'vitest';

import type { Artifact } from '@/entities/artifact';

import { buildArtifactUploadPlan, keepBothFileNames } from './useArtifactUpload';

const contents: Artifact[] = [
  { key: 'docs/report.txt', size: 10, lastModified: '2026-01-01T00:00:00Z', bucket: 'files' },
  { key: 'docs/report - Copy.txt', size: 10, lastModified: '2026-01-01T00:00:00Z', bucket: 'files' },
];

describe('artifact upload planning', () => {
  it('filters invalid and oversized files while finding scoped duplicates', () => {
    const duplicate = new File(['ok'], 'report.txt');
    const invalid = new File(['bad'], 'bad#.txt');
    const oversized = new File(['large'], 'large.txt');
    Object.defineProperty(oversized, 'size', { value: 1000 });
    const plan = buildArtifactUploadPlan(
      [duplicate, invalid, oversized],
      contents,
      '',
      'docs/',
      100,
    );
    expect(plan.accepted).toEqual([duplicate]);
    expect(plan.duplicates).toEqual(['report.txt']);
    expect(plan.rejected.map((issue) => issue.file.name)).toEqual(['bad#.txt', 'large.txt']);
    expect(plan.targetPrefix).toBe('docs/');
  });

  it('rejects an unsafe path', () => {
    expect(() => buildArtifactUploadPlan([new File(['x'], 'x.txt')], [], '../bad', '', 100)).toThrow();
  });

  it('renames collisions with Windows-style copy suffixes', () => {
    const files = [
      new File(['a'], 'report.txt', { type: 'text/plain', lastModified: 1 }),
      new File(['b'], 'fresh.txt'),
      new File(['c'], 'report.txt'),
    ];
    expect(keepBothFileNames(files, contents, 'docs/').map((file) => file.name)).toEqual([
      'report - Copy (2).txt',
      'fresh.txt',
      'report - Copy (3).txt',
    ]);
  });

  /* elitea_issues: #5358 — files sharing a base name but with different extensions must not be flagged as duplicates (full key, not basename, is compared) */
  it('does not flag same-base-name files with different extensions as duplicates', () => {
    const cpp = new File(['a'], 'sample.cpp');
    const cs = new File(['b'], 'sample.cs');
    const csv = new File(['c'], 'sample.csv');
    const plan = buildArtifactUploadPlan(
      [cpp, cs, csv],
      [{ key: 'docs/sample.cpp', size: 1, lastModified: '2026-01-01T00:00:00Z', bucket: 'files' }],
      '',
      'docs/',
      100,
    );
    expect(plan.accepted).toEqual([cpp, cs, csv]);
    expect(plan.duplicates).toEqual(['sample.cpp']);
  });
});
