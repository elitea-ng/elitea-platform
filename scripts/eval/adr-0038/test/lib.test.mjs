import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { validate, scoreChecks, scoreRubric } from '../lib/schema.mjs';
import { thinkingParams, buildBody, parseUsage } from '../lib/provider.mjs';
import { parseJsonReply } from '../lib/json.mjs';
import { testScore } from '../lib/tasks.mjs';
import { createRun, destroyRun, execTool, runCommand, safePath, sandboxKindAvailable } from '../lib/sandbox.mjs';

test('schema subset: types, required, additionalProperties, enum, formats', () => {
  const schema = {
    type: 'object',
    additionalProperties: false,
    required: ['a', 'd'],
    properties: {
      a: { type: 'integer', minimum: 1 },
      b: { type: ['string', 'null'], enum: ['x', 'y', null] },
      d: { type: 'string', format: 'date' },
      t: { type: 'string', format: 'date-time' },
      l: { type: 'array', minItems: 1, items: { type: 'number' } },
      p: { type: 'string', pattern: '^[A-Z]{3}$' },
    },
  };
  assert.deepEqual(validate(schema, { a: 2, b: null, d: '2026-02-28', t: '2026-02-28T10:00:00Z', l: [1.5], p: 'LHR' }), []);
  const errs = validate(schema, { a: 1.5, b: 'z', d: '2026-02-30', t: '2026-02-28 10:00', l: [], p: 'lhr', q: 1 });
  assert.equal(errs.length, 7, errs.join('\n'));
  assert.match(validate(schema, { d: '2026-01-01' }).join(), /missing required property a/);
});

test('expected-value checks', () => {
  const data = { items: [{ n: 'Apple ', q: 1.5 }], tags: ['b', 'A'], x: { y: 1 } };
  const s = scoreChecks([
    { path: 'items.0.n', op: 'str_ci', value: 'apple' },
    { path: 'items.0.q', op: 'num', value: 1.505 },
    { path: 'tags', op: 'set', value: ['a', 'B'] },
    { path: 'items', op: 'len', value: 1 },
    { path: 'x', op: 'eq', value: { y: 1 } },
    { path: 'x.z', op: 'eq', value: 2 },
  ], data);
  assert.equal(s.passed, 5);
  assert.equal(s.failed[0].path, 'x.z');
});

test('doc rubric', () => {
  const text = '# Runbook\n\n## Rollback\nRun `kubectl rollout undo`. Port 8443.\n';
  const s = scoreRubric([
    { kind: 'heading', pattern: '^#{1,3}\\s*Rollback' },
    { kind: 'regex', pattern: '8443' },
    { kind: 'absent', pattern: 'chore', flags: 'i' },
    { kind: 'max_words', value: 5 },
  ], text);
  assert.equal(s.passed, 3);
  assert.equal(s.failed[0].kind, 'max_words');
});

test('thinking params per control kind', () => {
  assert.deepEqual(thinkingParams({ kind: 'toggle' }, false), { chat_template_kwargs: { enable_thinking: false } });
  assert.deepEqual(thinkingParams({ kind: 'effort', on: 'high' }, true), { reasoning_effort: 'high' });
  assert.deepEqual(thinkingParams({ kind: 'effort' }, false), { reasoning_effort: 'none' });
  assert.deepEqual(thinkingParams({ kind: 'budget', budget: 1000 }, true), { thinking: { type: 'enabled', budget_tokens: 1000 } });
  assert.deepEqual(thinkingParams({ kind: 'budget' }, false), {});
  assert.equal(thinkingParams({ kind: 'budget', style: 'gemini' }, false).extra_body.google.thinking_config.thinking_budget, 0);
  const body = buildBody({ model: 'm', temperature: null, max_tokens_field: 'max_completion_tokens', thinking: { kind: 'toggle' } }, { messages: [], thinking: true, maxTokens: 5 });
  assert.equal(body.max_completion_tokens, 5);
  assert.equal('temperature' in body, false);
  assert.deepEqual(parseUsage({ prompt_tokens: 3, completion_tokens: 4, completion_tokens_details: { reasoning_tokens: 2 } }), { prompt: 3, completion: 4, reasoning: 2 });
});

test('lenient JSON reply parsing', () => {
  assert.deepEqual(parseJsonReply('Sure:\n```json\n{"a": 1}\n```'), { a: 1 });
  assert.deepEqual(parseJsonReply('x {"a": "}{", "b": {"c": 2}} y'), { a: '}{', b: { c: 2 } });
  assert.equal(parseJsonReply('no json'), null);
});

test('test score from runner output', () => {
  assert.equal(testScore('ℹ pass 7\nℹ fail 3', 1), 4);
  assert.equal(testScore('# pass 2\n# fail 1', 1), 1);
  assert.equal(testScore('--- FAIL: TestA\n--- FAIL: TestB\nFAIL', 1), -2);
  assert.equal(testScore('', 0), 1e6);
});

test('tools stay inside the workspace and commands are bounded', async () => {
  const seed = mkdtempSync(join(tmpdir(), 'adr0038-seed-'));
  writeFileSync(join(seed, 'a.txt'), 'hello');
  const run = createRun(seed);
  try {
    assert.throws(() => safePath(run.ws, '../x'));
    assert.equal((await execTool(run, 'read_file', '{"path":"a.txt"}')).text, 'hello');
    assert.equal((await execTool(run, 'read_file', '{"path":"/etc/passwd"}')).ok, false);
    assert.equal((await execTool(run, 'write_file', '{"path":"d/b.txt","content":"x"}')).ok, true);
    assert.match((await execTool(run, 'list_files', '{}')).text, /d\/b\.txt/);
    assert.equal((await execTool(run, 'nope', '{}')).ok, false);
    assert.equal((await execTool(run, 'read_file', '{bad')).ok, false);
    const r = await runCommand(run, 'sleep 5', { timeoutMs: 300 });
    assert.equal(r.timedOut, true);
    const big = await runCommand(run, 'yes | head -c 50000', { maxOutput: 1000 });
    assert.ok(big.output.length < 1200);
    if (sandboxKindAvailable() !== 'none') {
      const net = await runCommand(run, `node -e "fetch('http://example.com').then(()=>console.log('OPEN'),()=>console.log('BLOCKED'))"`, { timeoutMs: 20_000 });
      assert.match(net.output, /BLOCKED/);
    }
  } finally {
    destroyRun(run);
    rmSync(seed, { recursive: true, force: true });
  }
});
