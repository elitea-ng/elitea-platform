/**
 * The control catalogue is read by enterprise risk reviewers, so its status
 * column must not overclaim. Two rules, checked on every catalogue row:
 *
 * - `Planned` cites the public tracking issue, so a reader can follow it.
 * - `Partial` states its limit right after the word (a parenthesis or a
 *   colon), so "partly there" never stands alone.
 */
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const CATALOGUE = fileURLToPath(new URL('../src/content/docs/admin/enterprise-controls/overview.mdx', import.meta.url));
const STATUSES = ['Available', 'Partial', 'Planned'];

const rows = readFileSync(CATALOGUE, 'utf8')
  .split('\n')
  .filter((line) => line.startsWith('| ') && !line.startsWith('| Control') && !line.startsWith('| ---'))
  .map((line) => {
    const cells = line.split(' | ').map((cell) => cell.replace(/^\| ?| ?\|$/g, ''));
    return { control: cells[0] ?? '', status: cells[2] ?? '' };
  });

describe('enterprise control catalogue', () => {
  it('has rows', () => {
    assert.ok(rows.length > 20, `only ${rows.length} rows`);
  });

  for (const { control, status } of rows) {
    it(`${control} has a known status`, () => {
      assert.ok(STATUSES.some((s) => status.startsWith(s)), status);
    });
    if (status.startsWith('Planned')) {
      it(`${control} (Planned) cites its tracking issue`, () => {
        assert.match(status, /github\.com\/elitea-ng\/elitea-platform\/issues\/\d+/);
      });
    }
    if (status.startsWith('Partial')) {
      it(`${control} (Partial) states its limit`, () => {
        assert.match(status, /^Partial[ ]?[(:]/);
      });
    }
  }
});
