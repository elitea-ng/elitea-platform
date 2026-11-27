/**
 * Table test for elitea/status-chip (ADR-0062 decision 4). Each case is a file
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
const NAMED = "import { Chip } from '@mui/material';\n";

const CASES = [
  ['a coloured chip', `${NAMED}export const A = () => <Chip color="success" label="Active" />;`, 'src/features/foo/ui', 1],
  ['a colour from a variable', `${NAMED}export const A = ({ c }) => <Chip color={c} label="x" />;`, 'src/pages/admin', 1],
  ['an outlined coloured chip', `${NAMED}export const A = () => <Chip variant="outlined" color="error" label="x" />;`, 'src/features/foo/ui', 1],
  ['the default import', "import Chip from '@mui/material/Chip';\nexport const A = () => <Chip color=\"warning\" label=\"x\" />;", 'src/features/foo/ui', 1],
  ['a renamed import', "import { Chip as Pill } from '@mui/material';\nexport const A = () => <Pill color=\"info\" label=\"x\" />;", 'src/features/foo/ui', 1],
  ['a chip with no colour is a tag', `${NAMED}export const A = () => <Chip label="Lead" />;`, 'src/features/foo/ui', 0],
  ['color="default" is a tag', `${NAMED}export const A = () => <Chip color="default" label="x" />;`, 'src/features/foo/ui', 0],
  ['color={"default"} is a tag', `${NAMED}export const A = () => <Chip color={'default'} label="x" />;`, 'src/features/foo/ui', 0],
  ['another component named Chip', "import { Chip } from '@/shared/ui/chip';\nexport const A = () => <Chip color=\"success\" />;", 'src/features/foo/ui', 0],
  ['a type-only import', "import type { Chip } from '@mui/material';\nexport type A = typeof Chip;", 'src/features/foo/ui', 0],
  ['StatusChip itself in shared/ui', `${NAMED}export const A = () => <Chip color="success" label="x" />;`, 'src/shared/ui/status-chip', 0],
];

let results;

beforeAll(() => {
  const root = mkdtempSync(join(tmpdir(), 'status-chip-'));
  CASES.forEach(([, source, folder], i) => {
    const file = join(root, folder, `Case${i}.tsx`);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, `${source}\n`);
  });
  const run = spawnSync(OXLINT, ['-c', RATCHET_CONFIG, '-f', 'json', '--no-ignore', 'src'], { cwd: root, encoding: 'utf8' });
  rmSync(root, { recursive: true, force: true });
  results = CASES.map(() => []);
  for (const d of JSON.parse(run.stdout).diagnostics) {
    if (d.message.includes('[status-chip]')) results[Number(/Case(\d+)/.exec(d.filename)[1])].push(d.message);
  }
  // One real oxlint run over every case; under a loaded machine it takes longer than vitest's 10 s hook default.
}, 120_000);

describe('status chip', () => {
  CASES.forEach(([name, , , count], i) => {
    it(name, () => expect(results[i], results[i].join('\n')).toHaveLength(count));
  });
});
