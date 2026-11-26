/**
 * Table test for elitea/page-h1 (ADR-0062 decision 4). Each case is a file
 * linted by the real oxlint with scripts/style-ratchet.oxlintrc.json, so the
 * AST and the shared/ui override are oxlint's own.
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
const IMPORTS = "import { Typography } from '@mui/material';\n";

const CASES = [
  ['component="h1" on Typography', 'export const A = () => <Typography variant="headingLarge" component="h1">x</Typography>;', 'src/pages/admin', 1],
  ['component={"h1"}', 'export const A = () => <Typography component={"h1"}>x</Typography>;', 'src/features/foo/ui', 1],
  ['a native h1', 'export const A = () => <h1>x</h1>;', 'src/features/foo/ui', 1],
  ['an h2 is not a page title', 'export const A = () => <Typography component="h2">x</Typography>;', 'src/features/foo/ui', 0],
  ['a heading variant without h1', 'export const A = () => <Typography variant="headingLarge">x</Typography>;', 'src/features/foo/ui', 0],
  ['PageHeader itself in shared/ui', 'export const A = () => <Typography component="h1">x</Typography>;', 'src/shared/ui/page-frame', 0],
];

let results;

beforeAll(() => {
  const root = mkdtempSync(join(tmpdir(), 'page-h1-'));
  CASES.forEach(([, source, folder], i) => {
    const file = join(root, folder, `Case${i}.tsx`);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, `${IMPORTS}${source}\n`);
  });
  const run = spawnSync(OXLINT, ['-c', RATCHET_CONFIG, '-f', 'json', '--no-ignore', 'src'], { cwd: root, encoding: 'utf8' });
  rmSync(root, { recursive: true, force: true });
  results = CASES.map(() => []);
  for (const d of JSON.parse(run.stdout).diagnostics) {
    if (d.message.includes('[page-h1]')) results[Number(/Case(\d+)/.exec(d.filename)[1])].push(d.message);
  }
});

describe('page h1', () => {
  CASES.forEach(([name, , , count], i) => {
    it(name, () => expect(results[i], results[i].join('\n')).toHaveLength(count));
  });
});
