import { describe, expect, it } from 'vitest';

import {
  KINDS,
  UNPAIRED_FIXES,
  UNPAIRED_MANIFEST,
  UNPAIRED_NOTE,
  compareCounts,
  countFindings,
  folderOf,
  formatCounts,
  formatGrowth,
  isCounted,
  kindOf,
  namedUnpaired,
  oxlintArgs,
  parseNameStatus,
  parseOxlintJson,
  parseUnpairedManifest,
  planBaseTree,
  totals,
  unpairedFiles,
} from './style-ratchet-core.mjs';

const diag = (filename, message, line = 1, help) => ({
  filename,
  message,
  code: 'elitea(raw-px-spacing)',
  ...(help ? { help } : {}),
  labels: [{ span: { line } }],
});
const rem = (filename, line) => diag(filename, "R-T9 [spacing-rem] p: '1rem' → 2", line);
const dialog = (filename) => ({
  filename,
  message: "'Dialog' import from '@mui/material' is restricted.",
  code: 'eslint(no-restricted-imports)',
  help: '[mui-dialog] use BaseModal from shared/ui',
});

describe('isCounted', () => {
  it('counts screens under src/', () => {
    expect(isCounted('src/features/foo/ui/A.tsx')).toBe(true);
    expect(isCounted('src/main.tsx')).toBe(true);
  });

  it('skips tests, stories, mocks, declarations, the assistant vendor tree and files outside src/', () => {
    for (const path of [
      'src/features/foo/ui/A.test.tsx',
      'src/features/foo/ui/A.spec.ts',
      'src/features/foo/ui/A.stories.tsx',
      'src/features/foo/__tests__/helper.ts',
      'src/features/foo/__mocks__/api.ts',
      'src/test/setup.ts',
      'src/smoke/boot.ts',
      'src/vite-env.d.ts',
      'src/widgets/support-assistant/vendor/theme/colors.theme.ts',
      'index.html',
    ]) {
      expect(isCounted(path), path).toBe(false);
    }
  });
});

describe('folderOf', () => {
  it('is src/<layer>/<slice>', () => {
    expect(folderOf('src/features/foo/ui/deep/A.tsx')).toBe('src/features/foo');
    expect(folderOf('src/shared/ui/BaseModal/BaseModal.tsx')).toBe('src/shared/ui');
  });

  it('keeps a file right under a layer, or right under src, at that level', () => {
    expect(folderOf('src/app/App.tsx')).toBe('src/app');
    expect(folderOf('src/main.tsx')).toBe('src');
  });
});

describe('kindOf', () => {
  it('reads the bracketed kind from the message or from the help text', () => {
    expect(kindOf(rem('src/a/b/c.tsx'))).toBe('spacing-rem');
    expect(kindOf(dialog('src/a/b/c.tsx'))).toBe('mui-dialog');
  });

  it('reads a kind with a digit in its name', () => {
    expect(kindOf({ message: '[page-h1] a page title comes from PageHeader' })).toBe('page-h1');
  });

  it('is null for an untagged message or an unknown tag', () => {
    expect(kindOf({ message: 'R-T9: raw px' })).toBeNull();
    expect(kindOf({ message: '[not-a-kind] x' })).toBeNull();
    expect(kindOf({})).toBeNull();
  });
});

describe('parseOxlintJson', () => {
  it('returns the diagnostics', () => {
    expect(parseOxlintJson(JSON.stringify({ diagnostics: [rem('src/a/b/c.tsx')] }))).toHaveLength(1);
  });

  it('throws on output that is not a report, so a crash never reads as zero findings', () => {
    expect(() => parseOxlintJson('Failed to parse configuration')).toThrow(/did not print a JSON report/);
    expect(() => parseOxlintJson('{"files": 3}')).toThrow(/no diagnostics array/);
    expect(() => parseOxlintJson('null')).toThrow(/no diagnostics array/);
  });
});

describe('countFindings', () => {
  it('counts per folder and kind, and keeps the findings per file', () => {
    const counts = countFindings([
      rem('src/features/foo/ui/A.tsx', 3),
      rem('src/features/foo/lib/b.ts', 7),
      { ...dialog('src\\features\\foo\\ui\\A.tsx') },
      rem('src/features/foo/ui/A.test.tsx'),
      diag('src/features/foo/ui/A.tsx', "R-T9 [spacing-px] gap: '8px'"),
    ]);
    const foo = counts.get('src/features/foo');
    expect(foo.get('spacing-rem').count).toBe(2);
    expect(foo.get('mui-dialog').count).toBe(1);
    expect(foo.get('spacing-px').files.get('src/features/foo/ui/A.tsx')).toEqual([
      { line: 1, message: "R-T9 [spacing-px] gap: '8px'" },
    ]);
    expect(foo.get('mui-dialog').files.get('src/features/foo/ui/A.tsx')[0].line).toBe(0);
    expect(foo.get('spacing-rem').files.get('src/features/foo/lib/b.ts')[0].line).toBe(7);
  });

  it('throws on an untagged finding in a counted file', () => {
    expect(() => countFindings([diag('src/features/foo/ui/A.tsx', 'R-T9: raw px')])).toThrow(/untagged finding/);
  });
});

describe('compareCounts', () => {
  const base = countFindings([rem('src/features/foo/ui/A.tsx', 1), rem('src/features/foo/ui/A.tsx', 2)]);

  it('passes an equal or lower count', () => {
    expect(
      compareCounts(base, countFindings([rem('src/features/foo/ui/B.tsx'), rem('src/features/foo/ui/A.tsx')])),
    ).toEqual([]);
    expect(compareCounts(base, countFindings([]))).toEqual([]);
  });

  it('fails a folder whose count grew and names the files that grew', () => {
    const head = countFindings([
      rem('src/features/foo/ui/A.tsx', 1),
      rem('src/features/foo/ui/A.tsx', 2),
      rem('src/features/foo/ui/B.tsx', 9),
      dialog('src/features/zed/ui/C.tsx'),
    ]);
    expect(compareCounts(base, head)).toEqual([
      {
        folder: 'src/features/foo',
        kind: 'spacing-rem',
        before: 2,
        after: 3,
        files: [
          {
            path: 'src/features/foo/ui/B.tsx',
            before: 0,
            hits: [{ line: 9, message: "R-T9 [spacing-rem] p: '1rem' → 2" }],
          },
        ],
      },
      {
        folder: 'src/features/zed',
        kind: 'mui-dialog',
        before: 0,
        after: 1,
        files: [
          {
            path: 'src/features/zed/ui/C.tsx',
            before: 0,
            hits: [{ line: 0, message: `${dialog('').message} ${dialog('').help}` }],
          },
        ],
      },
    ]);
  });

  it('starts a new folder at 0 and reports a grown file that had findings before', () => {
    const head = countFindings([
      rem('src/features/foo/ui/A.tsx', 1),
      rem('src/features/foo/ui/A.tsx', 2),
      rem('src/features/foo/ui/A.tsx', 3),
    ]);
    const [growth] = compareCounts(base, head);
    expect(growth.files).toEqual([expect.objectContaining({ path: 'src/features/foo/ui/A.tsx', before: 2 })]);
  });
});

describe('totals and formatting', () => {
  const counts = countFindings([rem('src/features/foo/ui/A.tsx', 4), dialog('src/app/App.tsx')]);

  it('sums every kind, zero included', () => {
    expect(totals(counts)).toEqual({
      ...Object.fromEntries(KINDS.map((k) => [k, 0])),
      'spacing-rem': 1,
      'mui-dialog': 1,
    });
  });

  it('prints counts per folder in a stable order, with the total', () => {
    expect(formatCounts(counts)).toEqual([
      'src/app: mui-dialog 1',
      'src/features/foo: spacing-rem 1',
      'total: spacing-rem 1, spacing-px 0, spacing-off-scale 0, mui-dialog 1, mui-switch 0, legacy-palette 0, page-h1 0',
    ]);
  });

  it('prints each grown folder with its grown files and their findings', () => {
    const growth = compareCounts(countFindings([]), counts);
    expect(formatGrowth(growth)).toEqual([
      'src/app mui-dialog: 0 → 1 (the base branch is the ceiling)',
      '  src/app/App.tsx (0 → 1 in this file)',
      `    src/app/App.tsx:0 ${dialog('').message} ${dialog('').help}`,
      'src/features/foo spacing-rem: 0 → 1 (the base branch is the ceiling)',
      '  src/features/foo/ui/A.tsx (0 → 1 in this file)',
      "    src/features/foo/ui/A.tsx:4 R-T9 [spacing-rem] p: '1rem' → 2",
    ]);
  });
});

describe('planBaseTree', () => {
  const prefix = 'apps/elitea-web/';
  const baseFiles = [
    'apps/elitea-web/src/features/foo/ui/A.tsx',
    'apps/elitea-web/src/features/foo/ui/Moved.tsx',
    'apps/elitea-web/src/features/foo/ui/Leaves.tsx',
    'apps/elitea-web/src/features/foo/ui/Same.tsx',
    'libs/other.ts',
  ];
  const nameStatus = [
    'M\tapps/elitea-web/src/features/foo/ui/A.tsx',
    'R087\tapps/elitea-web/src/features/foo/ui/Moved.tsx\tapps/elitea-web/src/features/bar/ui/Moved.tsx',
    'R100\tapps/elitea-web/src/features/foo/ui/Leaves.tsx\tapps/elitea-web/tools/Leaves.tsx',
    'D\tapps/elitea-web/src/features/foo/ui/Gone.tsx',
    '',
  ].join('\n');

  it('writes a renamed file under its head path and every other file where it was', () => {
    expect(planBaseTree(baseFiles, nameStatus, prefix)).toEqual([
      { from: 'apps/elitea-web/src/features/foo/ui/A.tsx', to: 'src/features/foo/ui/A.tsx' },
      { from: 'apps/elitea-web/src/features/foo/ui/Moved.tsx', to: 'src/features/bar/ui/Moved.tsx' },
      { from: 'apps/elitea-web/src/features/foo/ui/Same.tsx', to: 'src/features/foo/ui/Same.tsx' },
    ]);
  });
});

describe("a split and a merge follow git's pairing (decision 3.2)", () => {
  const prefix = 'apps/elitea-web/';
  // A split: git pairs Big.tsx with one part (R) and lists the other as added.
  // A merge: git pairs the result with one source (R) and lists the other as deleted.
  const nameStatus = [
    'R064\tapps/elitea-web/src/features/foo/ui/Big.tsx\tapps/elitea-web/src/features/bar/ui/PartA.tsx',
    'A\tapps/elitea-web/src/features/bar/ui/PartB.tsx',
    'R071\tapps/elitea-web/src/features/foo/ui/One.tsx\tapps/elitea-web/src/features/zed/ui/Both.tsx',
    'D\tapps/elitea-web/src/features/foo/ui/Two.tsx',
  ].join('\n');
  const baseFiles = [
    'apps/elitea-web/src/features/foo/ui/Big.tsx',
    'apps/elitea-web/src/features/foo/ui/One.tsx',
    'apps/elitea-web/src/features/foo/ui/Two.tsx',
  ];

  it('writes the one paired part or source under its head path, and leaves the rest where it was', () => {
    expect(planBaseTree(baseFiles, nameStatus, prefix)).toEqual([
      { from: 'apps/elitea-web/src/features/foo/ui/Big.tsx', to: 'src/features/bar/ui/PartA.tsx' },
      { from: 'apps/elitea-web/src/features/foo/ui/One.tsx', to: 'src/features/zed/ui/Both.tsx' },
      { from: 'apps/elitea-web/src/features/foo/ui/Two.tsx', to: 'src/features/foo/ui/Two.tsx' },
    ]);
  });

  it('lists the other part as new and the other source as removed', () => {
    expect(unpairedFiles(nameStatus, prefix)).toEqual({
      added: ['src/features/bar/ui/PartB.tsx'],
      removed: ['src/features/foo/ui/Two.tsx'],
    });
  });
});

describe('parseNameStatus', () => {
  it('reads the status letter and both paths; a line with one path has from = to', () => {
    expect(parseNameStatus('R087\ta.tsx\tb.tsx\nM\tc.tsx\n\n')).toEqual([
      { status: 'R', from: 'a.tsx', to: 'b.tsx' },
      { status: 'M', from: 'c.tsx', to: 'c.tsx' },
    ]);
  });
});

describe('unpairedFiles', () => {
  const prefix = 'apps/elitea-web/';

  it('is every added and deleted file under src/, sorted, and no modified or renamed one', () => {
    const nameStatus = [
      'A\tapps/elitea-web/src/features/foo/ui/Z.tsx',
      'A\tapps/elitea-web/src/features/foo/ui/New.tsx',
      'D\tapps/elitea-web/src/features/foo/ui/Old.tsx',
      'M\tapps/elitea-web/src/features/foo/ui/Same.tsx',
      'R100\tapps/elitea-web/src/features/foo/ui/A.tsx\tapps/elitea-web/src/features/bar/ui/A.tsx',
      'A\tapps/elitea-web/index.html',
      'D\tapps/elitea-web/vite.config.ts',
    ].join('\n');
    expect(unpairedFiles(nameStatus, prefix)).toEqual({
      added: ['src/features/foo/ui/New.tsx', 'src/features/foo/ui/Z.tsx'],
      removed: ['src/features/foo/ui/Old.tsx'],
    });
  });

  it('counts a pair across the edge of src/ as new to, or removed from, the counted tree', () => {
    const nameStatus = [
      'R100\tapps/elitea-web/tools/In.tsx\tapps/elitea-web/src/features/foo/ui/In.tsx',
      'R100\tapps/elitea-web/src/features/foo/ui/Out.tsx\tapps/elitea-web/tools/Out.tsx',
    ].join('\n');
    expect(unpairedFiles(nameStatus, prefix)).toEqual({
      added: ['src/features/foo/ui/In.tsx'],
      removed: ['src/features/foo/ui/Out.tsx'],
    });
  });

  it('counts a pair out of, or into, a path the ratchet does not count as new or removed', () => {
    const nameStatus = [
      'R100\tapps/elitea-web/src/widgets/support-assistant/vendor/Pane.tsx\tapps/elitea-web/src/features/foo/ui/Pane.tsx',
      'R100\tapps/elitea-web/src/features/foo/ui/Card.tsx\tapps/elitea-web/src/features/foo/ui/Card.stories.tsx',
    ].join('\n');
    expect(unpairedFiles(nameStatus, prefix)).toEqual({
      added: ['src/features/foo/ui/Pane.tsx'],
      removed: ['src/features/foo/ui/Card.tsx'],
    });
    // On the base tree the vendor file stays at its old path, where it is not
    // counted, so its values count as new at the head.
    const baseFiles = [
      'apps/elitea-web/src/widgets/support-assistant/vendor/Pane.tsx',
      'apps/elitea-web/src/features/foo/ui/Card.tsx',
    ];
    expect(planBaseTree(baseFiles, nameStatus, prefix)).toEqual([
      {
        from: 'apps/elitea-web/src/widgets/support-assistant/vendor/Pane.tsx',
        to: 'src/widgets/support-assistant/vendor/Pane.tsx',
      },
      { from: 'apps/elitea-web/src/features/foo/ui/Card.tsx', to: 'src/features/foo/ui/Card.stories.tsx' },
    ]);
  });
});

describe('parseUnpairedManifest', () => {
  it('returns the two lists', () => {
    expect(parseUnpairedManifest('{"base":"x","added":["src/a/b/c.tsx"],"removed":[]}')).toEqual({
      added: ['src/a/b/c.tsx'],
      removed: [],
    });
  });

  it('throws on text that is not the manifest, so the names are never dropped quietly', () => {
    expect(() => parseUnpairedManifest('nope')).toThrow(`${UNPAIRED_MANIFEST} is not JSON`);
    expect(() => parseUnpairedManifest('null')).toThrow(/needs "added" and "removed"/);
    expect(() => parseUnpairedManifest('{"added":[]}')).toThrow(/needs "added" and "removed"/);
    expect(() => parseUnpairedManifest('{"added":[1],"removed":[]}')).toThrow(/needs "added" and "removed"/);
  });
});

describe('a grown folder with a file git did not pair', () => {
  const growth = compareCounts(
    countFindings([rem('src/features/bar/ui/Old.tsx', 1)]),
    countFindings([
      rem('src/features/bar/ui/Old.tsx', 1),
      rem('src/features/bar/ui/Moved.tsx', 3),
      rem('src/features/bar/ui/Moved.tsx', 4),
      rem('src/features/bar/ui/Edited.tsx', 2),
    ]),
  );
  const unpaired = { added: ['src/features/bar/ui/Moved.tsx', 'src/features/bar/ui/Clean.tsx'], removed: [] };

  it('names the new file in the message and gives the two fixes once for the folder', () => {
    expect(formatGrowth(growth, unpaired)).toEqual([
      'src/features/bar spacing-rem: 1 → 4 (the base branch is the ceiling)',
      `  src/features/bar/ui/Moved.tsx (0 → 2 in this file; ${UNPAIRED_NOTE})`,
      "    src/features/bar/ui/Moved.tsx:3 R-T9 [spacing-rem] p: '1rem' → 2",
      "    src/features/bar/ui/Moved.tsx:4 R-T9 [spacing-rem] p: '1rem' → 2",
      '  src/features/bar/ui/Edited.tsx (0 → 1 in this file)',
      "    src/features/bar/ui/Edited.tsx:2 R-T9 [spacing-rem] p: '1rem' → 2",
      ...UNPAIRED_FIXES.map((line) => `  ${line}`),
    ]);
    expect(UNPAIRED_FIXES.join(' ')).toMatch(
      /land each move unchanged in its own PR .* onto the system; a change of pixels goes first in its own PR/,
    );
  });

  it('lists the named files with their folder, kind and count; a new file with no values is not named', () => {
    expect(namedUnpaired(growth, unpaired)).toEqual([
      { folder: 'src/features/bar', kind: 'spacing-rem', path: 'src/features/bar/ui/Moved.tsx', count: 2 },
    ]);
  });

  it('has no fixes when no grown file is new', () => {
    expect(
      formatGrowth(growth, { added: ['src/features/zed/ui/X.tsx'] }).some((l) => l.includes('land each move')),
    ).toBe(false);
  });
});

describe('oxlintArgs', () => {
  it('runs one config over src/ with JSON output and no ignore files', () => {
    expect(oxlintArgs('/x/style-ratchet.oxlintrc.json')).toEqual([
      '-c',
      '/x/style-ratchet.oxlintrc.json',
      '-f',
      'json',
      '--no-ignore',
      'src',
    ]);
  });
});
