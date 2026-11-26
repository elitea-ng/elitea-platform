import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it } from 'node:test';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const PAGE = join(ROOT, 'src/content/docs/reference/audit-event-schema.mdx');
const PAGE_TEXT = readFileSync(PAGE, 'utf8');

// Read the schema straight from the repo, not the docs.
const SCHEMA = JSON.parse(readFileSync(new URL('../../../libs/jsonschema/audit/v1/event.schema.json', import.meta.url)), 'utf8');

describe('audit event schema page', () => {
  it('states the unknown-field rule', () => {
    assert.match(PAGE_TEXT, /receiver ignores fields it does not know/i);
  });

  it('names every signal.* field of the schema', () => {
    const signalProps = Object.keys(SCHEMA.properties.signal.properties);
    assert.ok(signalProps.length >= 8, 'the schema lost its signal object');
    for (const name of signalProps) {
      assert.ok(
        PAGE_TEXT.includes(`signal.${name}`),
        `the reference page does not mention signal.${name}`,
      );
    }
  });

  it('names the new context fields', () => {
    assert.ok(PAGE_TEXT.includes('role'), 'the reference page does not mention actor.role');
    assert.ok(PAGE_TEXT.includes('conversation_id'), 'the reference page does not mention correlation.conversation_id');
    assert.ok(PAGE_TEXT.includes('release_id'), 'the reference page does not mention target.release_id');
  });

  it('documents the chain.checkpoint event and the unknown-field rule', () => {
    assert.ok(PAGE_TEXT.includes('chain.checkpoint'), 'the reference page does not document chain.checkpoint');
  });
});
