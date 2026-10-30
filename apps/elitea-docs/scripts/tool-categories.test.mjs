/**
 * The Category column of the toolkit reference pages is generated from
 * libs/proto/contracts/tool-categories-v1.json (scripts/tool-categories.mjs).
 * This check fails when a page does not match the contract.
 */
import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { NOTE, renderPage, run, toolId } from './tool-categories.mjs';

const contract = {
  categories: ['read', 'write', 'delete', 'external_send'],
  default_category: 'write',
  toolkits: { demo: { get_item: 'read', delete_item: 'delete' } },
};

const page = (table) => `# Demo\n\n<Accordion title="Available tools (3)">\n\n${table}\n</Accordion>\n`;

describe('tool categories in the toolkit reference', () => {
  it('every page matches the contract', () => {
    const { changed, problems } = run({ write: false });
    assert.deepEqual(problems, []);
    assert.deepEqual(changed, [], 'run "node scripts/tool-categories.mjs" in apps/elitea-docs');
  });

  it('maps a display name to its tool id', () => {
    assert.equal(toolId('Apply git patch'), 'apply_git_patch');
  });

  it('adds the column, with the default for an unlisted tool', () => {
    const { text, problems } = renderPage(
      page('| Tool |\n|---|\n| Get item |\n| Delete item |\n| Create item |\n'),
      'demo',
      contract,
    );
    assert.deepEqual(problems, []);
    assert.ok(text.includes(`${NOTE}\n\n| Tool | Category |`));
    assert.ok(text.includes('| Get item | `read` |'));
    assert.ok(text.includes('| Delete item | `delete` |'));
    assert.ok(text.includes('| Create item | `write` |'));
  });

  it('is idempotent and corrects a hand edit', () => {
    const first = renderPage(page('| Tool |\n|---|\n| Get item |\n| Delete item |\n'), 'demo', contract).text;
    assert.equal(renderPage(first, 'demo', contract).text, first);
    const edited = first.replace('| Delete item | `delete` |', '| Delete item | `read` |');
    assert.equal(renderPage(edited, 'demo', contract).text, first);
  });

  it('reports a contract tool that the page does not list', () => {
    const { problems } = renderPage(page('| Tool |\n|---|\n| Get item |\n'), 'demo', contract);
    assert.equal(problems.length, 1);
    assert.match(problems[0], /demo\.delete_item/);
  });
});
