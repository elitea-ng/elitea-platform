import { describe, expect, it } from 'vitest';

import fixture from './fixtures/code-debug-public-trace.json';
import { codeDebugToolMeta, MAX_CODE_DEBUG_BYTES, parseCodeDebugProof } from './codeDebugArtifact';

const first = fixture.first_attempt_history.metadata.code_debug_v1;
const retry = fixture.retry_committed.metadata.code_debug_v1;

function changed(key: string, value: unknown): Record<string, unknown> {
  return { ...structuredClone(first), [key]: value };
}

describe('frozen Code debug consumer-contract2', () => {
  it('keeps a new retry separate from first-attempt history', () => {
    expect(parseCodeDebugProof(first)).toEqual(first);
    expect(parseCodeDebugProof(retry)).toEqual(retry);
    expect(retry.original_visit).not.toEqual(first.original_visit);
    expect(retry.attempt).toBe(2);
    expect(retry.artifact.name).not.toBe(first.artifact.name);
    expect(fixture.retry_committed.run_id).not.toBe(fixture.first_attempt_history.run_id);
    expect(codeDebugToolMeta(fixture.retry_current_warning)['code_debug_v1']).toEqual(fixture.retry_current_warning.metadata.code_debug_v1);
  });

  it('accepts the real bounded execution grammar instead of recovery selectors', () => {
    expect(parseCodeDebugProof(changed('execution_id', 'execution-1'))).toBeDefined();
    expect(parseCodeDebugProof(changed('execution_id', 'é'.repeat(128)))).toBeDefined();
    expect(parseCodeDebugProof(changed('execution_id', 'é'.repeat(129)))).toBeUndefined();
    expect(parseCodeDebugProof(changed('generation', '18446744073709551615'))).toBeDefined();
    expect(parseCodeDebugProof(changed('generation', '18446744073709551616'))).toBeUndefined();
  });

  it.each([
    ['execution_id', ''], ['execution_id', 'a\u0000b'], ['execution_id', '\uD800'],
    ['generation', '0'], ['generation', '07'], ['generation', 7], ['generation', '+7'], ['generation', '7\n'],
    ['node_id', 'node with spaces'], ['node_id', 'x'.repeat(129)], ['node_id', 'é'], ['node_id', 'run\n'],
    ['attempt', 0], ['attempt', 17], ['attempt', 1.5], ['revision', 2],
    ['activation_id', 'f'.repeat(63)], ['activation_id', `${'f'.repeat(64)}\n`], ['request_sha256', 'F'.repeat(64)],
    ['status', 'failed'], ['raw_source', 'private source'],
  ])('refuses malformed %s metadata', (key, value) => {
    expect(parseCodeDebugProof(changed(key, value))).toBeUndefined();
  });

  it.each([
    { ...first.original_visit, visit_id: '0'.repeat(64) },
    { ...first.original_visit, digest_sha256: '0'.repeat(64) },
    { ...first.original_visit, revision: 2 },
    { ...first.original_visit, grant: 'not authority' },
  ])('refuses an invalid original visit', visit => {
    expect(parseCodeDebugProof(changed('original_visit', visit))).toBeUndefined();
  });

  it.each([
    ['project_id', 0], ['project_id', 2147483648], ['byte_length', 0], ['byte_length', MAX_CODE_DEBUG_BYTES + 1],
    ['name', '../source.json'], ['name', `${first.artifact.name}\n`], ['name', `${'F'.repeat(64)}.json`], ['bucket', 'other'],
    ['media_type', 'text/html'], ['sha256', 'f'.repeat(63)], ['url', 'https://example.invalid/private'],
  ])('refuses malformed artifact %s', (key, value) => {
    expect(parseCodeDebugProof(changed('artifact', { ...first.artifact, [key]: value }))).toBeUndefined();
  });

  it('requires a committed ref and forbids a denied or unavailable ref', () => {
    const without: Record<string, unknown> = { ...first };
    delete without['artifact'];
    expect(parseCodeDebugProof(without)).toBeUndefined();
    expect(parseCodeDebugProof({ ...without, status: 'denied' })).toBeDefined();
    expect(parseCodeDebugProof({ ...first, status: 'denied' })).toBeUndefined();
    expect(parseCodeDebugProof({ ...without, status: 'unavailable', artifact: null })).toBeUndefined();
  });

  it('requires both exact Main metadata copies and Code node identity', () => {
    expect(codeDebugToolMeta(fixture.first_attempt_history)).toEqual({ code_debug_v1: first });
    const conflict = structuredClone(fixture.first_attempt_history);
    conflict.tool_meta.metadata.code_debug_v1.attempt = 2;
    expect(codeDebugToolMeta(conflict)).toEqual({ code_debug_v1: null });
    const wrongNode = structuredClone(fixture.first_attempt_history);
    wrongNode.metadata.langgraph_node = 'another';
    expect(codeDebugToolMeta(wrongNode)).toEqual({ code_debug_v1: null });
    expect(codeDebugToolMeta({ metadata: fixture.first_attempt_history.metadata })).toEqual({ code_debug_v1: null });
    expect(codeDebugToolMeta({ metadata: { toolkit_name: 'ordinary' } })).toEqual({});
  });
});
