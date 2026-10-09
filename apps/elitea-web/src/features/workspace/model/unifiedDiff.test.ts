import { describe, expect, it } from 'vitest';

import { parseUnifiedDiff } from './unifiedDiff';

const DIFF = [
  'diff --git a/a.txt b/a.txt',
  'index 111..222 100644',
  '--- a/a.txt',
  '+++ b/a.txt',
  '@@ -1,3 +1,3 @@',
  ' keep',
  '-old',
  '+new',
  ' tail',
  '\\ No newline at end of file',
  '',
].join('\n');

describe('parseUnifiedDiff', () => {
  it('drops file headers and keeps hunk headers, context, additions and removals in order', () => {
    expect(parseUnifiedDiff(DIFF)).toEqual([
      { kind: 'unchanged', lines: ['@@ -1,3 +1,3 @@', 'keep'] },
      { kind: 'removed', lines: ['old'] },
      { kind: 'added', lines: ['new'] },
      { kind: 'unchanged', lines: ['tail', '\\ No newline at end of file'] },
    ]);
  });

  it('does not mistake a removed line that starts with dashes for a header inside a hunk', () => {
    const parts = parseUnifiedDiff(['@@ -1 +0,0 @@', '--- not a header'].join('\n'));
    expect(parts[1]).toEqual({ kind: 'removed', lines: ['-- not a header'] });
  });

  it('returns nothing for an empty diff', () => {
    expect(parseUnifiedDiff('')).toEqual([]);
  });
});
