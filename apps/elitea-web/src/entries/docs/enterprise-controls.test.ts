/**
 * The control catalogue is read by enterprise risk reviewers, so its status
 * column must not overclaim. Two rules, checked on every catalogue row:
 *
 * - `Planned` cites the public tracking issue, so a reader can follow it.
 * - `Partial` states its limit right after the word (a parenthesis or a
 *   colon), so "partly there" never stands alone.
 */
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const CATALOGUE = join(
  dirname(fileURLToPath(import.meta.url)),
  'content/admin/enterprise-controls/overview.mdx',
);
const STATUSES = ['Available', 'Partial', 'Planned'] as const;

interface Row {
  control: string;
  status: string;
}

function catalogueRows(): Row[] {
  return readFileSync(CATALOGUE, 'utf8')
    .split('\n')
    .filter((line) => line.startsWith('| ') && !line.startsWith('| Control') && !line.startsWith('| ---'))
    .map((line) => {
      const cells = line.split(' | ').map((cell) => cell.replace(/^\| ?| ?\|$/g, ''));
      return { control: cells[0] ?? '', status: cells[2] ?? '' };
    });
}

describe('enterprise control catalogue', () => {
  const rows = catalogueRows();

  it('has rows', () => {
    expect(rows.length).toBeGreaterThan(20);
  });

  it.each(rows)('$control has a known status', ({ status }) => {
    expect(STATUSES.some((s) => status.startsWith(s))).toBe(true);
  });

  it.each(rows.filter((r) => r.status.startsWith('Planned')))('$control (Planned) cites its tracking issue', ({ status }) => {
    expect(status).toMatch(/github\.com\/elitea-ng\/elitea-platform\/issues\/\d+/);
  });

  it.each(rows.filter((r) => r.status.startsWith('Partial')))('$control (Partial) states its limit', ({ status }) => {
    expect(status).toMatch(/^Partial[ ]?[(:]/);
  });
});
