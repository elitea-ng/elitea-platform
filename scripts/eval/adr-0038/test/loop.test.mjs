import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, readFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createRun, destroyRun } from '../lib/sandbox.mjs';
import { runVerifiedLoop } from '../lib/loop.mjs';
import { runAgent, thinkingFor } from '../lib/agent.mjs';

const usage = { prompt: 10, completion: 5, reasoning: 0 };

function seedDir() {
  const d = mkdtempSync(join(tmpdir(), 'adr0038-test-'));
  mkdirSync(d, { recursive: true });
  writeFileSync(join(d, 'out.txt'), '0');
  return d;
}

// A fake model. The worker writes `values[round]` to out.txt and finishes.
// The manager's review is given by `review(round)`.
function fakeChat({ values, review, workerAlwaysTools = false }) {
  let round = 0;
  const log = [];
  const chat = async (messages, opts) => {
    const sys = messages[0].content;
    if (opts.tools) {
      log.push({ kind: 'worker', thinking: opts.thinking });
      const last = messages[messages.length - 1];
      if (workerAlwaysTools || last.role === 'user') {
        if (last.role === 'user') round++;
        return {
          message: { role: 'assistant', content: '', tool_calls: [{ id: `c${round}`, type: 'function', function: { name: 'write_file', arguments: JSON.stringify({ path: 'out.txt', content: String(values[round - 1]) }) } }] },
          finish: 'tool_calls', usage, ms: 1,
        };
      }
      return { message: { role: 'assistant', content: `round ${round} done` }, finish: 'stop', usage, ms: 1 };
    }
    if (sys.startsWith('You are the manager')) {
      const user = messages[1].content;
      if (user.includes('Write the plan')) {
        log.push({ kind: 'plan', thinking: opts.thinking });
        return { message: { content: JSON.stringify({ acceptance_criteria: ['out.txt is right'], tasks: ['t1'], first_task: 'task 1' }) }, finish: 'stop', usage, ms: 1 };
      }
      log.push({ kind: 'review', thinking: opts.thinking });
      return { message: { content: JSON.stringify(review(round)) }, finish: 'stop', usage, ms: 1 };
    }
    log.push({ kind: sys.startsWith('Write the final') ? 'finish' : 'summary', thinking: opts.thinking });
    return { message: { content: 'summary' }, finish: 'stop', usage, ms: 1 };
  };
  return { chat, log };
}

function verifierFrom(fn, strong = true) {
  let n = 0;
  return {
    kind: 'command', strong,
    describe: () => 'out.txt must be right',
    async run(run) {
      n++;
      const v = readFileSync(join(run.ws, 'out.txt'), 'utf8');
      return fn(v, n);
    },
  };
}

const task = { prompt: 'make out.txt right' };

async function withRun(fn) {
  const seed = seedDir();
  const run = createRun(seed);
  try { return await fn(run); } finally { destroyRun(run); rmSync(seed, { recursive: true, force: true }); }
}

test('a manager done while the strong verifier fails is overridden', async () => {
  await withRun(async (run) => {
    const { chat } = fakeChat({ values: [1, 2, 3], review: (r) => ({ decision: 'done', next_task: `task ${r + 1}` }) });
    const verifier = verifierFrom(() => ({ pass: false, score: 0, detail: 'fail' }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 3 });
    assert.equal(r.outcome, 'unverified');
    assert.equal(r.stop, 'round_limit');
    assert.equal(r.rounds, 3);
    assert.equal(r.overrides, 2);
  });
});

test('a passing result is never replaced by a failing one', async () => {
  await withRun(async (run) => {
    const { chat } = fakeChat({ values: [1, 2, 3], review: (r) => ({ decision: 'continue', next_task: `task ${r + 1}` }) });
    const verifier = verifierFrom((v) => ({ pass: v === '1', score: v === '1' ? 1e6 : 0, detail: v }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 3 });
    assert.equal(r.outcome, 'verified');
    assert.ok(r.restores >= 1);
    assert.equal(readFileSync(join(run.ws, 'out.txt'), 'utf8'), '1');
  });
});

test('the same task issued twice in a row stops the run', async () => {
  await withRun(async (run) => {
    const { chat } = fakeChat({ values: [1, 2, 3], review: () => ({ decision: 'continue', next_task: 'Task 1.' }) });
    const verifier = verifierFrom(() => ({ pass: false, score: 0, detail: 'fail' }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 8 });
    assert.equal(r.stop, 'same_task');
    assert.equal(r.rounds, 1);
  });
});

test('at the round limit the best failing candidate is returned unverified', async () => {
  await withRun(async (run) => {
    const { chat } = fakeChat({ values: [5, 9, 3], review: (r) => ({ decision: 'continue', next_task: `task ${r + 1}` }) });
    const verifier = verifierFrom((v) => ({ pass: false, score: Number(v), detail: v }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 3 });
    assert.equal(r.outcome, 'unverified');
    assert.equal(readFileSync(join(run.ws, 'out.txt'), 'utf8'), '9');
  });
});

test('a pass on changed check files never becomes the best candidate', async () => {
  await withRun(async (run) => {
    const { chat } = fakeChat({ values: [1, 3, 2], review: (r) => ({ decision: 'continue', next_task: `task ${r + 1}` }) });
    // Round 1 "passes" by changing the check; rounds 2 and 3 fail honestly.
    const verifier = verifierFrom((v) => (v === '1'
      ? { pass: false, rawPass: true, changed: ['test/a.test.mjs'], score: 1e6, detail: 'verifier changed' }
      : { pass: false, score: Number(v), detail: v }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 3 });
    assert.equal(r.outcome, 'verifier_changed');
    assert.equal(readFileSync(join(run.ws, 'out.txt'), 'utf8'), '3');
  });
});

test('a cut-off worker gets a summary call with thinking off', async () => {
  await withRun(async (run) => {
    const { chat, log } = fakeChat({ values: [1, 1, 1, 1], review: () => ({ decision: 'done' }), workerAlwaysTools: true });
    const verifier = verifierFrom(() => ({ pass: true, score: 1e6, detail: 'ok' }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 2, workerSteps: 2 });
    assert.equal(r.cutOffs, 1);
    const summary = log.find((l) => l.kind === 'summary');
    assert.ok(summary);
    assert.equal(summary.thinking, false);
    assert.equal(r.outcome, 'verified');
  });
});

test('a pass that does not repeat on the confirm re-run is not verified', async () => {
  await withRun(async (run) => {
    const { chat } = fakeChat({ values: [1, 2], review: (r) => ({ decision: 'done', next_task: `task ${r + 1}` }) });
    // Call 1 is the baseline; afterwards only odd calls pass.
    const verifier = verifierFrom((v, n) => ({ pass: n > 1 && n % 2 === 0, score: 0, detail: 'x' }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 2 });
    assert.notEqual(r.outcome, 'verified');
    assert.ok(r.flaky >= 1);
  });
});

test('the confirm re-run is for command verifiers only', async () => {
  for (const [kind, expectedCalls] of [['command', 3], ['schema', 2]]) {
    await withRun(async (run) => {
      const { chat } = fakeChat({ values: [1], review: () => ({ decision: 'done' }) });
      let calls = 0;
      // Baseline (1) + round 1 (2) + the confirm re-run for command (3).
      const verifier = { kind, strong: true, describe: () => 'x', async run() { calls++; return { pass: true, score: 1e6, detail: 'ok' }; } };
      const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 3 });
      assert.equal(r.outcome, 'verified');
      assert.equal(calls, expectedCalls, kind);
    });
  }
});

test('manager calls think; finish and summary do not', async () => {
  await withRun(async (run) => {
    const { chat, log } = fakeChat({ values: [1], review: () => ({ decision: 'done' }) });
    const verifier = verifierFrom(() => ({ pass: true, score: 1e6, detail: 'ok' }));
    const r = await runVerifiedLoop({ chat, run, task, verifier, maxRounds: 3 });
    assert.equal(r.stop, 'done');
    for (const l of log) {
      if (l.kind === 'plan' || l.kind === 'review') assert.equal(l.thinking, true);
      if (l.kind === 'finish') assert.equal(l.thinking, false);
    }
  });
});

test('auto policy: on for the first step and after a failure, off otherwise', () => {
  assert.equal(thinkingFor('auto', { step: 0, prevFailed: false }), true);
  assert.equal(thinkingFor('auto', { step: 3, prevFailed: false }), false);
  assert.equal(thinkingFor('auto', { step: 3, prevFailed: true }), true);
  assert.equal(thinkingFor('on', { step: 3, prevFailed: false }), true);
  assert.equal(thinkingFor('off', { step: 0, prevFailed: true }), false);
});

test('auto: a non-zero exit is data, a command timeout is a failure', async () => {
  await withRun(async (run) => {
    const script = [
      { name: 'run_command', args: { command: 'grep -q nothing out.txt' } }, // step 0 (on): exit 1, data
      { name: 'run_command', args: { command: 'exit 3' } }, // step 1 (off): exit 3, data
      { name: 'list_files', args: {} }, // step 2 (off)
      null,
    ];
    const seen = [];
    let i = 0;
    const chat = async (_m, opts) => {
      seen.push(opts.thinking);
      const s = script[i++];
      if (!s) return { message: { content: 'done' }, finish: 'stop', usage, ms: 1 };
      return { message: { content: '', tool_calls: [{ id: `x${i}`, type: 'function', function: { name: s.name, arguments: JSON.stringify(s.args) } }] }, finish: 'tool_calls', usage, ms: 1 };
    };
    const r = await runAgent({ chat, run, user: 'go', policy: 'auto' });
    assert.deepEqual(seen, [true, false, false, false]);
    assert.equal(r.toolErrors, 0);
    // auto-exit thinks after each non-zero exit.
    seen.length = 0;
    i = 0;
    await runAgent({ chat, run, user: 'go', policy: 'auto-exit' });
    assert.deepEqual(seen, [true, true, true, false]);
    // A timeout is a tool failure: the next step thinks.
    const seen2 = [];
    let j = 0;
    const timeoutExec = async () => ({ ok: false, text: 'timed out after 60 s (exit 124)' });
    const chat2 = async (_m, opts) => {
      seen2.push(opts.thinking);
      if (j++ < 2) return { message: { content: '', tool_calls: [{ id: `t${j}`, type: 'function', function: { name: 'run_command', arguments: '{"command":"sleep 100"}' } }] }, finish: 'tool_calls', usage, ms: 1 };
      return { message: { content: 'done' }, finish: 'stop', usage, ms: 1 };
    };
    await runAgent({ chat: chat2, run, user: 'go', policy: 'auto', exec: timeoutExec });
    assert.deepEqual(seen2, [true, true, true]);
  });
});

test('runAgent switches thinking per step under auto', async () => {
  await withRun(async (run) => {
    const script = [
      { name: 'read_file', args: { path: 'missing.txt' } }, // step 0 (on) -> error
      { name: 'read_file', args: { path: 'out.txt' } }, // step 1 (on, after error) -> ok
      { name: 'list_files', args: {} }, // step 2 (off)
      null, // step 3 (off): final
    ];
    const seen = [];
    let i = 0;
    const chat = async (_m, opts) => {
      seen.push(opts.thinking);
      const s = script[i++];
      if (!s) return { message: { content: 'done' }, finish: 'stop', usage, ms: 1 };
      return { message: { content: '', tool_calls: [{ id: `x${i}`, type: 'function', function: { name: s.name, arguments: JSON.stringify(s.args) } }] }, finish: 'tool_calls', usage, ms: 1 };
    };
    const r = await runAgent({ chat, run, user: 'go', policy: 'auto' });
    assert.deepEqual(seen, [true, true, false, false]);
    assert.equal(r.toolErrors, 1);
    assert.equal(r.final, 'done');
  });
});
