/**
 * Table test for elitea/raw-px-spacing in ratchet mode (ADR-0062 decision 3.4)
 * and for the unchanged default mode. Each case is a file linted by the real
 * oxlint with scripts/style-ratchet.oxlintrc.json (or a default-mode config),
 * so the AST, the parent links and the import handling are oxlint's own.
 *
 *   FINDINGS — each source must give exactly the listed kinds, in order.
 *   EXEMPT   — each source must give nothing. Some are exempt by design (a
 *              size, a chart's own px prop); one is a known blind spot and is
 *              listed so a change to it is seen: an untyped object outside
 *              JSX whose name is not `sx` or `…Sx` is not known to be sx, so
 *              its numbers are not counted (the counts are a floor, as
 *              ADR-0062 says).
 *   An expectation is a prefix of the message after its tag; the "the fix
 *   names …" cases give the whole message.
 */
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

const APP = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const OXLINT = join(APP, 'node_modules', '.bin', 'oxlint');
const RATCHET_CONFIG = join(APP, 'scripts', 'style-ratchet.oxlintrc.json');

const IMPORTS =
  "import { Box, Stack, styled } from '@mui/material';\nimport type { SxProps, Theme } from '@mui/material';\n";

const FINDINGS = [
  ['rem in sx', "export const A = () => <Box sx={{ p: '1rem' }} />;", ["spacing-rem p: '1rem' → 2"]],
  [
    'two lengths in one value',
    "export const A = () => <Box sx={{ padding: '0.5rem 1rem' }} />;",
    ["spacing-rem padding: '0.5rem' → 1", "spacing-rem padding: '1rem' → 2"],
  ],
  [
    'a negative rem margin',
    "export const A = () => <Box sx={{ marginLeft: '-0.5rem' }} />;",
    ["spacing-rem marginLeft: '-0.5rem' → -1"],
  ],
  [
    'rem off the scale',
    "export const A = () => <Box sx={{ gap: '0.3rem' }} />;",
    ["spacing-rem gap: '0.3rem' is 0.6 units, off the scale → nearest steps 0.5 or 1"],
  ],
  [
    'an off-scale number in sx',
    'export const A = () => <Box sx={{ p: 0.75 }} />;',
    ['spacing-off-scale p: 0.75 is off the scale → nearest steps 0.5 or 1'],
  ],
  ['an off-scale JSX spacing prop', 'export const A = () => <Box p={0.75} />;', ['spacing-off-scale p: 0.75']],
  [
    'Stack spacing off the scale',
    'export const A = () => <Stack spacing={2.5} />;',
    ['spacing-off-scale spacing: 2.5 is off the scale → nearest steps 2 or 3'],
  ],
  [
    'a rem JSX spacing prop',
    'export const A = () => <Stack columnSpacing="2rem" />;',
    ["spacing-rem columnSpacing: '2rem' → 4"],
  ],
  [
    'theme.spacing() in a spacing value',
    'export const A = () => <Box sx={(t) => ({ padding: t.spacing(1.25, 2) })} />;',
    ['spacing-off-scale padding → spacing(): 1.25'],
  ],
  [
    'theme.spacing() inside a template value',
    'export const A = () => <Box sx={(t) => ({ padding: `${t.spacing(0.75)} 0` })} />;',
    ['spacing-off-scale padding → spacing(): 0.75'],
  ],
  [
    'both branches of a conditional',
    'export const A = ({ o }) => <Box sx={{ p: o ? 0.75 : 1.25 }} />;',
    ['spacing-off-scale p: 0.75', 'spacing-off-scale p: 1.25'],
  ],
  [
    'a responsive object',
    'export const A = () => <Box sx={{ p: { xs: 0.75, md: 2 } }} />;',
    ['spacing-off-scale p: 0.75'],
  ],
  ['a responsive array', 'export const A = () => <Box sx={{ p: [2, 0.25] }} />;', ['spacing-off-scale p: 0.25']],
  [
    'an sx object declared outside JSX',
    'export const s: SxProps<Theme> = { gap: 0.75 };',
    ['spacing-off-scale gap: 0.75'],
  ],
  [
    'a map of sx objects',
    'export const s: Record<string, SxProps<Theme>> = { a: { gap: 0.75 } };',
    ['spacing-off-scale gap: 0.75'],
  ],
  ['satisfies SxProps', 'export const s = { gap: 0.75 } satisfies SxProps;', ['spacing-off-scale gap: 0.75']],
  [
    'a function typed to return sx',
    'export const f = (): SxProps<Theme> => ({ mt: 0.25 });',
    ['spacing-off-scale mt: 0.25'],
  ],
  [
    'the paddingX alias',
    'export const A = () => <Box sx={{ paddingX: 0.75 }} />;',
    ['spacing-off-scale paddingX: 0.75'],
  ],
  [
    'an sx callback value',
    'export const A = () => <Box sx={{ gap: (t) => t.spacing(0.25) }} />;',
    ['spacing-off-scale gap → spacing(): 0.25'],
  ],
  [
    'an sx callback with a block body',
    'export const A = () => <Box sx={{ gap: (t) => { return t.spacing(0.25); } }} />;',
    ['spacing-off-scale gap → spacing(): 0.25'],
  ],
  [
    'sx inside slotProps',
    'export const A = () => <Box slotProps={{ root: { sx: { p: 0.75 } } }} />;',
    ['spacing-off-scale p: 0.75'],
  ],
  [
    'a bare number in a styled() object is px',
    'export const S = styled(Box)({ padding: 12 });',
    ['spacing-px padding: 12 is px here (a styled() object or a style prop) → 1.5'],
  ],
  [
    'a bare number in a styled() callback is px',
    "export const S = styled('div')(({ theme }) => ({ gap: 3 }));",
    ['spacing-px gap: 3 is px here (a styled() object or a style prop) is 0.375 units, off the scale'],
  ],
  [
    'a bare number in a style prop is px',
    'export const A = () => <div style={{ padding: 4 }} />;',
    ['spacing-px padding: 4'],
  ],
  ['a px string anywhere', "export const x = { padding: '8px' };", ["spacing-px padding: '8px' → 1"]],
  [
    'px in a css template',
    'export const S = styled.div`\n  padding: 12px 1rem;\n`;',
    ["spacing-px css template: '12px' → 1.5", "spacing-rem css template: '1rem' → 2"],
  ],
  ['a negative px string', "export const x = { margin: '-1px' };", ["spacing-px margin: '-1px' is -0.125 units"]],
  ['Dialog from the barrel', "import { Dialog } from '@mui/material';\nexport const D = Dialog;", ['mui-dialog']],
  [
    'Dialog renamed',
    "import { Dialog as MuiDialog } from '@mui/material';\nexport const D = MuiDialog;",
    ['mui-dialog'],
  ],
  ['Switch from its own path', "import Switch from '@mui/material/Switch';\nexport const W = Switch;", ['mui-switch']],
  ['Dialog from its own path', "import Dialog from '@mui/material/Dialog';\nexport const D = Dialog;", ['mui-dialog']],
  ['Switch from the barrel', "import { Switch } from '@mui/material';\nexport const W = Switch;", ['mui-switch']],
  ['an untyped object named …Sx', 'export const rowSx = { p: 0.75 };', ['spacing-off-scale p: 0.75']],
  ['an untyped object named sx', 'export const sx = { gap: 0.75 };', ['spacing-off-scale gap: 0.75']],
  ['a JSX prop named …Sx', 'export const A = () => <Box headerSx={{ p: 0.75 }} />;', ['spacing-off-scale p: 0.75']],
  ['a key named …Sx', 'export const props = { paperSx: { mt: 0.75 } };', ['spacing-off-scale mt: 0.75']],
  [
    'the fix in sx names the sx number',
    "export const A = () => <Box sx={{ p: '1rem' }} />;",
    ["spacing-rem p: '1rem' → 2 (theme.spacing(2) or the sx number 2)"],
  ],
  [
    'the fix in a styled() object names only theme.spacing()',
    "export const S = styled(Box)({ padding: '1rem', margin: 16 });",
    [
      "spacing-rem padding: '1rem' → 2 (theme.spacing(2))",
      'spacing-px margin: 16 is px here (a styled() object or a style prop) → 2 (theme.spacing(2))',
    ],
  ],
  [
    'the fix in a style prop names only theme.spacing()',
    "export const A = () => <div style={{ padding: '1rem' }} />;",
    ["spacing-rem padding: '1rem' → 2 (theme.spacing(2))"],
  ],
  [
    'the fix in a css template names a theme.spacing() interpolation',
    'export const S = styled.div`\n  padding: 1rem;\n`;',
    ["spacing-rem css template: '1rem' → 2 (${({ theme }) => theme.spacing(2)})"],
  ],
];

const EXEMPT = [
  ['steps on the scale', 'export const A = () => <Box sx={{ p: 1.5, m: -1, gap: 0, pt: 0.5 }} p={8} />;'],
  ['rem equal to nothing in a size', "export const A = () => <Box sx={{ width: '1rem', fontSize: '0.875rem' }} />;"],
  [
    'theme.spacing() in a size or a position',
    'export const A = () => <Box sx={(t) => ({ maxWidth: t.spacing(40), top: t.spacing(2.5) })} />;',
  ],
  [
    "a third-party part's own px prop",
    "import { Background } from '@xyflow/react';\nexport const A = () => <Background gap={20} />;",
  ],
  ['zero in a styled() object', 'export const S = styled(Box)({ padding: 0 });'],
  ['a type-only import of Dialog', "import type { DialogProps } from '@mui/material';\nexport type P = DialogProps;"],
  [
    'a type-only import from the Dialog path',
    "import type { DialogProps } from '@mui/material/Dialog';\nexport type P = DialogProps;",
  ],
  ['Dialog parts other than Dialog', "import { DialogTitle } from '@mui/material';\nexport const T = DialogTitle;"],
  ['known blind spot: an untyped object outside JSX not named …Sx', 'export const styles = { root: { p: 0.75 } };'],
];

let dir;
let results;

function lint(config, cases) {
  const root = mkdtempSync(join(tmpdir(), 'raw-px-spacing-'));
  cases.forEach(([, source], i) => {
    const file = join(root, 'src', 'features', `case${i}`, 'ui', 'Case.tsx');
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, `${IMPORTS}${source}\n`);
  });
  const run = spawnSync(OXLINT, ['-c', config, '-f', 'json', '--no-ignore', 'src'], { cwd: root, encoding: 'utf8' });
  rmSync(root, { recursive: true, force: true });
  const byCase = cases.map(() => []);
  for (const d of JSON.parse(run.stdout).diagnostics) {
    const i = Number(/case(\d+)/.exec(d.filename)[1]);
    byCase[i].push({
      line: d.labels[0].span.line,
      column: d.labels[0].span.column,
      text: `${d.message} ${d.help ?? ''}`,
    });
  }
  return byCase.map((hits) => hits.sort((a, b) => a.line - b.line || a.column - b.column).map((h) => h.text));
}

/** `[spacing-rem] p: '1rem' → 2` ⇒ the expectation `spacing-rem p: '1rem' → 2` must be a prefix after the tag. */
const matches = (text, expected) => {
  const [kind, ...rest] = expected.split(' ');
  const tagged = text.indexOf(`[${kind}]`);
  if (tagged < 0) return false;
  const after = text.slice(tagged + kind.length + 2).trim();
  return rest.length === 0 || after.startsWith(rest.join(' '));
};

beforeAll(() => {
  dir = mkdtempSync(join(tmpdir(), 'raw-px-spacing-cfg-'));
  results = lint(RATCHET_CONFIG, [...FINDINGS, ...EXEMPT]);
});

afterAll(() => rmSync(dir, { recursive: true, force: true }));

describe('ratchet mode: findings', () => {
  FINDINGS.forEach(([name, , expected], i) => {
    it(name, () => {
      const got = results[i];
      expect(got, got.join('\n')).toHaveLength(expected.length);
      expected.forEach((e, j) => expect(matches(got[j], e), `${got[j]}\n  should match: ${e}`).toBe(true));
    });
  });
});

describe('ratchet mode: exempt', () => {
  EXEMPT.forEach(([name], i) => {
    it(name, () => expect(results[FINDINGS.length + i]).toEqual([]));
  });
});

describe('default mode (no options) is unchanged', () => {
  it('reports px strings with the old message and nothing the ratchet added', () => {
    const config = join(dir, 'default.json');
    writeFileSync(
      config,
      JSON.stringify({
        plugins: [],
        jsPlugins: [join(APP, 'tools', 'lint-rules', 'index.mjs')],
        categories: { correctness: 'off' },
        rules: { 'elitea/raw-px-spacing': 'error' },
      }),
    );
    const [px, rem, number, alias, template] = lint(config, [
      ['px', "export const A = () => <Box sx={{ p: '8px' }} />;"],
      ['rem', "export const A = () => <Box sx={{ p: '1rem' }} />;"],
      ['number', 'export const A = () => <Box sx={{ p: 0.75 }} p={0.75} />;'],
      ['alias', "export const A = () => <Box sx={{ paddingX: '8px' }} />;"],
      ['template', 'export const S = styled.div`\n  padding: 12px;\n`;'],
    ]);
    expect(px).toEqual(['R-T9: p: "8px" — spacing is theme.spacing(n) or a unitless sx number, never raw px ']);
    expect(rem).toEqual([]);
    expect(number).toEqual([]);
    expect(alias).toEqual([]);
    expect(template).toEqual([
      'R-T9: raw px spacing inside a css template — spacing is theme.spacing(n), never raw px ',
    ]);
  });
});
