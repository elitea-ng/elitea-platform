/**
 * Table test for elitea/legacy-palette-id (R-T15, ADR-0062 decision 3.4 and
 * Verification → Legacy ids). Each case is a file linted by the real oxlint
 * with scripts/style-ratchet.oxlintrc.json, so the AST and the config's
 * overrides are oxlint's own.
 *
 *   FINDINGS — each source must give exactly the listed messages, in order.
 *   EXEMPT   — each source must give nothing.
 */
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { beforeAll, describe, expect, it } from 'vitest';

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const OXLINT = join(APP, 'node_modules', '.bin', 'oxlint');
const RATCHET_CONFIG = join(APP, 'scripts', 'style-ratchet.oxlintrc.json');

const IMPORTS =
  "import { Box, Typography } from '@mui/material';\nimport type { SxProps, Theme } from '@mui/material';\n";
const FEATURE = 'src/features/foo/ui';

const FINDINGS = [
  [
    'a vars read',
    'export const sx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });',
    ['text.secondary → role.text (theme.vars.palette.role.text)'],
  ],
  [
    'a read through an sx callback parameter',
    'export const A = () => <Box sx={(t) => ({ borderColor: t.vars.palette.border.lines })} />;',
    ['border.lines → role.border'],
  ],
  [
    'a read through destructured vars',
    'export const sx: SxProps<Theme> = ({ vars }) => ({ color: vars.palette.primary.main });',
    ['primary.main → role.primary'],
  ],
  [
    'a non-null vars read',
    'export const f = (theme: Theme) => theme.vars!.palette.text.primary;',
    ['text.primary → role.textMuted'],
  ],
  [
    'a CSS variable in a string',
    "export const c = 'var(--el-palette-text-secondary)';",
    ['text.secondary → role.text'],
  ],
  [
    'a CSS variable in a template',
    'export const b = `1px solid var(--el-palette-border-lines)`;',
    ['border.lines → role.border'],
  ],
  [
    'a palette string in sx',
    "export const A = () => <Box sx={{ color: 'text.primary', bgcolor: 'background.default' }} />;",
    ['text.primary → role.textMuted', 'background.default → role.bg'],
  ],
  [
    'a palette string as a JSX prop',
    'export const A = () => <Typography color="text.secondary">x</Typography>;',
    ['text.secondary → role.text'],
  ],
  [
    'both branches of a conditional',
    "export const A = ({ on }: { on: boolean }) => <Typography color={on ? 'error.main' : 'text.primary'}>x</Typography>;",
    ['error.main → role.dangerFill', 'text.primary → role.textMuted'],
  ],
];

const EXEMPT = [
  [
    'a component id (a tuning point, not on the map)',
    'export const sx: SxProps<Theme> = (theme) => ({ bgcolor: theme.vars.palette.background.tab.hover });',
  ],
  [
    'an id off the map although its colour is a role\'s (text.default)',
    "export const A = () => <Box sx={{ color: 'text.default' }} />;",
  ],
  [
    'an id off the map although its colour is a role\'s (focus.ring)',
    'export const sx: SxProps<Theme> = (theme) => ({ outlineColor: theme.vars.palette.focus.ring });',
  ],
  ['a role', 'export const sx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.role.textMuted });'],
  ['a role as a palette string', "export const A = () => <Box sx={{ color: 'role.text' }} />;"],
  ['a palette group, not an id', 'export const f = (theme: Theme) => theme.vars.palette.text;'],
  ['a MUI colour name', 'export const A = () => <Typography color="primary">x</Typography>;'],
  ['a channel variable', "export const c = 'rgba(var(--el-palette-text-secondaryChannel) / 0.5)';"],
  ['theme.palette (R-T7 covers it)', 'export const f = (theme: Theme) => theme.palette.text.secondary;'],
  ['an id in a prop that is not a colour', 'export const A = () => <Box title="text.secondary" />;'],
  [
    'the brand layer',
    'export const sx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });',
    'src/shared/brand/mui-overrides',
  ],
];

let results;

function lint(cases) {
  const root = mkdtempSync(join(tmpdir(), 'legacy-palette-id-'));
  cases.forEach(([, source, folder = FEATURE], i) => {
    const file = join(root, folder, `Case${i}.tsx`);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, `${IMPORTS}${source}\n`);
  });
  const run = spawnSync(OXLINT, ['-c', RATCHET_CONFIG, '-f', 'json', '--no-ignore', 'src'], {
    cwd: root,
    encoding: 'utf8',
  });
  rmSync(root, { recursive: true, force: true });
  const byCase = cases.map(() => []);
  for (const d of JSON.parse(run.stdout).diagnostics) {
    const i = Number(/Case(\d+)/.exec(d.filename)[1]);
    byCase[i].push({ line: d.labels[0].span.line, column: d.labels[0].span.column, text: d.message });
  }
  return byCase.map((hits) => hits.sort((a, b) => a.line - b.line || a.column - b.column).map((h) => h.text));
}

beforeAll(() => {
  // A finding's third field is its expectation; only EXEMPT cases name a folder.
  results = lint([...FINDINGS.map(([name, source]) => [name, source]), ...EXEMPT]);
});

describe('legacy palette ids: findings', () => {
  FINDINGS.forEach(([name, , expected], i) => {
    it(name, () => {
      const got = results[i];
      expect(got, got.join('\n')).toHaveLength(expected.length);
      expected.forEach((e, j) => expect(got[j]).toContain(`[legacy-palette] ${e}`));
    });
  });
});

describe('legacy palette ids: exempt', () => {
  EXEMPT.forEach(([name], i) => {
    it(name, () => expect(results[FINDINGS.length + i]).toEqual([]));
  });
});
