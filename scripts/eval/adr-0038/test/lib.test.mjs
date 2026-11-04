import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync, symlinkSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { validate, scoreChecks, scoreRubric } from '../lib/schema.mjs';
import { thinkingParams, buildBody, parseUsage } from '../lib/provider.mjs';
import { parseJsonReply } from '../lib/json.mjs';
import { testScore } from '../lib/tasks.mjs';
import { createRun, destroyRun, execTool, runCommand, safePath, sandboxKindAvailable, makeCapture } from '../lib/sandbox.mjs';

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
  // The first `{` does not start valid JSON; a later one does.
  assert.deepEqual(parseJsonReply('Use {curly} sets. Answer: {"decision": "done"}'), { decision: 'done' });
  assert.deepEqual(parseJsonReply('{ not json } then {"a": [1, {"b": 2}]}'), { a: [1, { b: 2 }] });
  // An unbalanced first `{`.
  assert.deepEqual(parseJsonReply('x { y {"ok": true}'), { ok: true });
});

test('test score from runner output', () => {
  assert.equal(testScore('ℹ pass 7\nℹ fail 3', 1), 4);
  assert.equal(testScore('# pass 2\n# fail 1', 1), 1);
  assert.equal(testScore('--- FAIL: TestA\n--- FAIL: TestB\nFAIL', 1), -2);
  assert.equal(testScore('', 0), 1e6);
});

test('output capture keeps a bounded head and the real end of the stream', async () => {
  const c = makeCapture(100);
  c.add('H'.repeat(30));
  for (let i = 0; i < 1000; i++) c.add('x'.repeat(97) + '\n\n\n');
  c.add('SUMMARY pass 7 fail 3');
  const t = c.text();
  assert.ok(t.startsWith('H'.repeat(30) + 'x'.repeat(10)));
  assert.ok(t.endsWith('SUMMARY pass 7 fail 3'));
  const cut = Number(t.match(/\[(\d+) chars cut\]/)[1]);
  assert.equal(cut, 30 + 100_000 + 21 - 100);
  const short = makeCapture(100);
  short.add('a'.repeat(60));
  short.add('b'.repeat(40));
  assert.equal(short.text(), 'a'.repeat(60) + 'b'.repeat(40));
  // A real command with output far beyond the old 20x collection limit.
  const seed = mkdtempSync(join(tmpdir(), 'adr0038-seed-'));
  const run = createRun(seed);
  try {
    const r = await runCommand(run, 'yes | head -c 400000; echo; echo "ℹ pass 5"', { maxOutput: 1000 });
    assert.match(r.output, /ℹ pass 5\s*$/);
    assert.match(r.output, /\[399\d+ chars cut\]/);
  } finally {
    destroyRun(run);
    rmSync(seed, { recursive: true, force: true });
  }
});

test('symlinks cannot lead read_file or write_file out of the workspace', async () => {
  const seed = mkdtempSync(join(tmpdir(), 'adr0038-seed-'));
  const outside = mkdtempSync(join(tmpdir(), 'adr0038-outside-'));
  writeFileSync(join(seed, 'a.txt'), 'hello');
  writeFileSync(join(outside, 'secret.txt'), 'secret');
  const run = createRun(seed);
  try {
    symlinkSync(outside, join(run.ws, 'dirlink'));
    symlinkSync(join(outside, 'secret.txt'), join(run.ws, 'filelink'));
    symlinkSync(join(outside, 'new.txt'), join(run.ws, 'dangling'));
    symlinkSync(join(run.ws, 'a.txt'), join(run.ws, 'innerlink'));
    for (const path of ['dirlink/secret.txt', 'filelink', 'dirlink/sub/new.txt']) {
      const r = await execTool(run, 'read_file', JSON.stringify({ path }));
      assert.equal(r.ok, false, path);
      assert.match(r.text, /outside the workspace/);
    }
    const w1 = await execTool(run, 'write_file', JSON.stringify({ path: 'dirlink/x.txt', content: 'x' }));
    const w2 = await execTool(run, 'write_file', JSON.stringify({ path: 'dangling', content: 'x' }));
    assert.equal(w1.ok, false);
    assert.equal(w2.ok, false);
    assert.equal(existsSync(join(outside, 'x.txt')), false);
    assert.equal(existsSync(join(outside, 'new.txt')), false);
    // A link that stays inside works.
    assert.equal((await execTool(run, 'read_file', '{"path":"innerlink"}')).text, 'hello');
    assert.doesNotMatch((await execTool(run, 'list_files', '{}')).text, /secret/);
  } finally {
    destroyRun(run);
    rmSync(seed, { recursive: true, force: true });
    rmSync(outside, { recursive: true, force: true });
  }
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
    assert.equal(r.toolFailed, true);
    // A non-zero exit is data for the model, not a tool failure.
    const exit3 = await execTool(run, 'run_command', '{"command":"echo boom; exit 3"}');
    assert.equal(exit3.ok, true);
    assert.match(exit3.text, /^exit code 3\nboom/);
    assert.equal((await runCommand(run, 'exit 3')).toolFailed, false);
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
