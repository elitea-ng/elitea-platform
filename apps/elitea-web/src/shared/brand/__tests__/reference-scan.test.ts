import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { colorsIn, scanThemeVarReferences } from './reference-scan';

/**
 * The scan is what makes §4.6 check 7 assertion (b) a gate rather than a
 * formality, so its failure modes get their own tests on a fixture tree.
 */
let root: string | null = null;

const fixture = (files: Record<string, string>): string => {
  root = mkdtempSync(join(tmpdir(), 'el-scan-'));
  for (const [name, source] of Object.entries(files)) {
    const path = join(root, name);
    mkdirSync(join(path, '..'), { recursive: true });
    writeFileSync(path, source, 'utf8');
  }
  return root;
};

afterEach(() => {
  if (root !== null) rmSync(root, { recursive: true, force: true });
  root = null;
});

describe('scanThemeVarReferences', () => {
  /*
   * The scan skips the Babel parse for files whose raw text cannot spell
   * `palette` (see CANDIDATE_RE). These two pin the escape clause that makes
   * that skip sound: both files reference a real token, and NEITHER contains
   * the literal seven characters, so a filter that only looked for `palette`
   * would silently stop protecting them.
   */
  it('sees a token read through a unicode-escaped identifier', () => {
    const dir = fixture({
      'a/Escaped.tsx': 'export const s = ({ theme }: never) => ({\n' +
        '  color: theme.vars.\\u0070alette.border.lines,\n});',
    });
    expect(scanThemeVarReferences(dir).map((r) => r.token)).toEqual(['border.lines']);
  });

  it('sees a custom property named with an escape in a string literal', () => {
    const dir = fixture({
      'a/Escaped.ts': "export const s = 'var(--el-\\x70alette-border-lines)';",
    });
    expect(scanThemeVarReferences(dir).map((r) => r.cssVar)).toEqual([
      '--el-palette-border-lines',
    ]);
  });

  it('skips a file that cannot spell the prefix, and finds one that can', () => {
    const dir = fixture({
      'a/Plain.ts': 'export const n = 1 + 2;',
      'a/Uses.ts': "export const s = 'var(--el-palette-border-lines)';",
    });
    expect(scanThemeVarReferences(dir).map((r) => r.file)).toEqual(['a/Uses.ts']);
  });

  it('records the longest static chain once, with file and line', () => {
    const dir = fixture({
      'a/Widget.tsx': [
        'export const s = ({ theme }: never) => ({',
        '  color: theme.vars.palette.text.button.primary,',
        '});',
      ].join('\n'),
      'a/skip.md': 'not source',
    });
    const found = scanThemeVarReferences(dir);
    expect(found).toEqual([
      {
        file: 'a/Widget.tsx',
        line: 2,
        token: 'text.button.primary',
        cssVar: '--el-palette-text-button-primary',
      },
    ]);
  });

  it('ignores member chains that are not theme.vars.palette', () => {
    const dir = fixture({ 'b.ts': 'export const x = a.b.c.d;\nexport const y = theme.vars.shape.radiusMd;\n' });
    expect(scanThemeVarReferences(dir)).toEqual([]);
  });

  it('refuses computed access, which would hide tokens from the gate', () => {
    const dir = fixture({ 'c.ts': 'export const x = (k: string) => theme.vars.palette[k];\n' });
    expect(() => scanThemeVarReferences(dir)).toThrow(/static dotted path/);
  });

  it('refuses an alias of theme.vars.palette for the same reason', () => {
    const dir = fixture({ 'd.ts': 'export const p = theme.vars.palette;\n' });
    expect(() => scanThemeVarReferences(dir)).toThrow(/static dotted path/);
  });

  it('walks nested directories and skips build output', () => {
    const dir = fixture({
      'deep/nested/e.ts': 'export const x = theme.vars.palette.border.lines;\n',
      'node_modules/pkg/f.ts': 'export const y = theme.vars.palette.border.hover;\n',
    });
    expect(scanThemeVarReferences(dir).map((r) => r.token)).toEqual(['border.lines']);
  });
});

describe('var(--el-*) literal detection (MEDIUM-1 adversarial gap)', () => {
  it('RED — catches a nonexistent token hidden in a plain string literal', () => {
    // This is the exact bypass adversarial verification found: a raw
    // 'var(--el-palette-…)' string never goes through a MemberExpression, so
    // before this fix scanThemeVarReferences returned [] for this fixture —
    // a typo'd token referenced this way shipped unverified.
    const dir = fixture({
      'g.ts': "export const sx = { color: 'var(--el-palette-nonexistent-token)' };\n",
    });
    expect(scanThemeVarReferences(dir)).toEqual([
      {
        file: 'g.ts',
        line: 1,
        token: 'palette.nonexistent.token',
        cssVar: '--el-palette-nonexistent-token',
      },
    ]);
  });

  it('RED — catches the same bypass hidden in a template literal', () => {
    const dir = fixture({
      'h.ts': 'export const sx = { color: `var(--el-palette-nonexistent-token)` };\n',
    });
    expect(scanThemeVarReferences(dir).map((r) => r.cssVar)).toEqual([
      '--el-palette-nonexistent-token',
    ]);
  });

  it('GREEN — a legitimate var(--el-*) literal round-trips to the real token', () => {
    const dir = fixture({ 'i.ts': 'export const sx = `var(--el-palette-text-secondary)`;\n' });
    expect(scanThemeVarReferences(dir)).toEqual([
      {
        file: 'i.ts',
        line: 1,
        token: 'palette.text.secondary',
        cssVar: '--el-palette-text-secondary',
      },
    ]);
  });

  it('shape/spacing literals are not caught by the literal regex (AST scans handle theme.vars.shape/*)', () => {
    const dir = fixture({ 'j.ts': "export const r = 'var(--el-shape-radiusLg, 16px)';\n" });
    expect(scanThemeVarReferences(dir)).toEqual([]);
  });

  it('does not mistake a single-dash attribute string for a CSS variable', () => {
    const dir = fixture({ 'k.ts': "export const attr = 'data-el-scheme';\n" });
    expect(scanThemeVarReferences(dir)).toEqual([]);
  });

  it('exempts test/story files, so their own --el- prose cannot self-trigger', () => {
    // brandPack.contract.test.ts's own `it(...)` description quotes
    // "--el-palette-*" verbatim, and buildTheme.test.ts checks a deliberately
    // incomplete `startsWith('var(--el-palette-')` prefix. Neither is a
    // component referencing a token; both are indistinguishable from a real
    // reference by regex alone, so literal scanning is scoped to non-test
    // files (MemberExpression scanning is NOT scoped — it stays universal).
    const dir = fixture({
      'widget.test.ts': "it('checks var(--el-palette-nonexistent-token)', () => {});\n",
      'widget.stories.tsx': "export const s = 'var(--el-palette-also-nonexistent)';\n",
    });
    expect(scanThemeVarReferences(dir)).toEqual([]);
  });
});

describe('colorsIn', () => {
  it('canonicalises every notation to lower-case hex', () => {
    const text = [`#${'FFF'}`, ' rgb', '(', '255, 255, 255)'].join('');
    expect([...colorsIn(text)]).toEqual([`#${'ffffff'}`]);
  });

  it('returns nothing for colourless text', () => {
    expect(colorsIn('0 0 8px inset').size).toBe(0);
  });
});
