/**
 * The reason of a dangerous administration action (ADR-0059 R3.8a, issue
 * 2539) reaches operators through two documents that must not go stale:
 *
 * - the upgrade note lists every marked (route, action) of the registry. The
 *   expected list is read from the registry's own golden of the action table
 *   (services/elitea-main/internal/adminaccess/testdata/actions.golden, which
 *   TestActionTableMatchesGolden keeps equal to the table), so a new marked
 *   entry fails here until the note names it;
 * - the deployment hardening checklist says access logs must not record the
 *   Elitea-Reason header.
 */
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const ROOT = new URL('../../../', import.meta.url);
const GOLDEN = fileURLToPath(new URL('services/elitea-main/internal/adminaccess/testdata/actions.golden', ROOT));
const UPGRADING = fileURLToPath(new URL('docs/upgrading/', ROOT));
const HARDENING = fileURLToPath(new URL('../src/content/docs/deployment/hardening-checklist.mdx', import.meta.url));

const marked = readFileSync(GOLDEN, 'utf8')
  .split('\n')
  .filter((line) => line && !line.startsWith('#'))
  .map((line) => line.split('\t'))
  .filter((cells) => cells[4] === 'true')
  .map(([method, route, action, danger]) => ({ method, route, action: action === '-' ? '' : action, danger }));

describe('reasons on dangerous administration actions', () => {
  it('the registry marks the actions of decision 6', () => {
    assert.ok(marked.length >= 30, `only ${marked.length} marked entries in ${GOLDEN}`);
  });

  const note = readdirSync(UPGRADING).find((name) => /^\d{4}-\d{2}-\d{2}-2539-reason-on-dangerous-actions\.md$/.test(name));

  it('has an upgrade note', () => {
    assert.ok(note, `no docs/upgrading/<date>-2539-reason-on-dangerous-actions.md`);
  });

  it('the upgrade note lists every marked route and action', () => {
    const rows = readFileSync(new URL(note, `file://${UPGRADING}`), 'utf8')
      .split('\n')
      .filter((line) => line.startsWith('| `'));
    for (const { method, route, action, danger } of marked) {
      const row = rows.find((line) => {
        const cells = line.split('|').map((cell) => cell.trim());
        const named = action === '' ? cells[2] === 'whole route' : cells[2] === `\`${action}\``;
        return cells[1] === `\`${method} ${route}\`` && named && cells[3] === danger;
      });
      assert.ok(row, `the upgrade note does not list ${method} ${route} ${action || '(whole route)'} as ${danger}`);
    }
    assert.equal(rows.length, marked.length, 'the upgrade note lists a route the registry does not mark');
  });

  it('the hardening checklist keeps Elitea-Reason out of access logs', () => {
    const text = readFileSync(HARDENING, 'utf8');
    assert.match(text, /access logs do not record the `Elitea-Reason` header/);
  });
});
