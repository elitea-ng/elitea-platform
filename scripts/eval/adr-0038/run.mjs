#!/usr/bin/env node
// ADR-0038 phase 0 evaluation runner.
//
//   node run.mjs --provider qwen [--variants a,b,c,d] [--repeat 2]
//                [--tasks <regex>] [--tasks-dir dir] [--concurrency 6]
//                [--out results/qwen-2026-11-04]
//   node run.mjs --selftest [--tasks <regex>]
//
// Variants: a = single run, thinking on; b = single run, thinking off;
// c = single run, auto policy; d = verified loop (auto workers, manager on).
import { appendFileSync, existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { loadProviders, makeClient } from './lib/provider.mjs';
import { createRun, destroyRun, overlay, sandboxKindAvailable } from './lib/sandbox.mjs';
import { loadTasks, makeVerifier, scoreFinal } from './lib/tasks.mjs';
import { runAgent } from './lib/agent.mjs';
import { runVerifiedLoop } from './lib/loop.mjs';
import { summarize } from './summarize.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));

const VARIANTS = {
  a: { name: 'single-on', policy: 'on' },
  b: { name: 'single-off', policy: 'off' },
  c: { name: 'single-auto', policy: 'auto' },
  d: { name: 'loop-auto', loop: true },
};

const { values: args } = parseArgs({
  options: {
    provider: { type: 'string', default: 'qwen' },
    providers: { type: 'string', default: join(HERE, 'providers.json') },
    'tasks-dir': { type: 'string', default: join(HERE, 'tasks') },
    variants: { type: 'string', default: 'a,b,c,d' },
    repeat: { type: 'string', default: '1' },
    tasks: { type: 'string' },
    concurrency: { type: 'string' },
    out: { type: 'string' },
    'max-steps': { type: 'string', default: '40' },
    'worker-steps': { type: 'string', default: '20' },
    'max-rounds': { type: 'string', default: '8' },
    selftest: { type: 'boolean', default: false },
  },
});

const tasks = loadTasks(args['tasks-dir'], args.tasks);

function sumCalls(calls) {
  const t = { calls: calls.length, thinking_calls: 0, prompt: 0, completion: 0, reasoning: 0, model_ms: 0 };
  for (const c of calls) {
    t.prompt += c.prompt;
    t.completion += c.completion;
    t.reasoning += c.reasoning;
    t.model_ms += c.ms;
    if (c.thinking) t.thinking_calls++;
  }
  return t;
}

function byRole(calls) {
  const out = {};
  for (const c of calls) {
    const r = (out[c.role] ??= { calls: 0, prompt: 0, completion: 0, reasoning: 0 });
    r.calls++;
    r.prompt += c.prompt;
    r.completion += c.completion;
    r.reasoning += c.reasoning;
  }
  return out;
}

async function selftest() {
  let bad = 0;
  for (const task of tasks) {
    const start = createRun(task.filesDir);
    const s0 = await scoreFinal(task, start);
    destroyRun(start);
    const sol = createRun(task.filesDir);
    overlay(join(task.dir, 'solution'), sol.ws);
    const s1 = await scoreFinal(task, sol);
    destroyRun(sol);
    const hiddenOk = task.hasHidden ? s1.hidden_pass === true : true;
    const ok = !s0.pass && s1.pass && hiddenOk;
    if (!ok) bad++;
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${task.id.padEnd(32)} start=${s0.pass ? 'pass' : 'fail'} solution=${s1.pass ? 'pass' : 'fail'}${task.hasHidden ? ` hidden=${s1.hidden_pass}` : ''}${ok ? '' : `\n${s1.detail}\n${s1.hidden_detail ?? ''}`}`);
  }
  console.log(`${tasks.length - bad}/${tasks.length} tasks well-formed; sandbox: ${sandboxKindAvailable()}`);
  process.exit(bad ? 1 : 0);
}

async function runJob(chat, task, vkey, rep) {
  const v = VARIANTS[vkey];
  const run = createRun(task.filesDir);
  const t0 = Date.now();
  const rec = { task: task.id, group: task.group, variant: vkey, variant_name: v.name, repeat: rep };
  try {
    if (v.loop) {
      const verifier = makeVerifier(task, { chat });
      const r = await runVerifiedLoop({
        chat, run, task, verifier,
        maxRounds: Number(args['max-rounds']), workerSteps: Number(args['worker-steps']),
      });
      Object.assign(rec, {
        outcome: r.outcome, stop: r.stop, rounds: r.rounds, overrides: r.overrides,
        cut_offs: r.cutOffs, restores: r.restores, flaky: r.flaky, events: r.events,
        criteria: r.criteria, tokens: sumCalls(r.calls), by_role: byRole(r.calls),
      });
    } else {
      const r = await runAgent({ chat, run, user: task.prompt, policy: v.policy, maxSteps: Number(args['max-steps']) });
      Object.assign(rec, {
        steps: r.steps, cut_off: r.cutOff, cut_reason: r.cutReason ?? null,
        tool_errors: r.toolErrors, tokens: sumCalls(r.calls),
      });
    }
    rec.wall_ms = Date.now() - t0;
    Object.assign(rec, await scoreFinal(task, run));
  } catch (err) {
    rec.error = String(err?.stack ?? err).slice(0, 2000);
    rec.wall_ms = Date.now() - t0;
    rec.pass = false;
  } finally {
    destroyRun(run);
  }
  return rec;
}

async function main() {
  if (args.selftest) return selftest();
  const providers = loadProviders(args.providers);
  const provider = providers[args.provider];
  if (!provider) throw new Error(`no provider ${args.provider} in ${args.providers}`);
  const chat = makeClient(provider);
  const date = new Date().toISOString().slice(0, 10);
  const out = args.out ?? join(HERE, 'results', `${args.provider}-${date}`);
  mkdirSync(dirname(out), { recursive: true });
  const jsonl = `${out}.jsonl`;
  const done = new Set();
  if (existsSync(jsonl)) {
    for (const line of readFileSync(jsonl, 'utf8').split('\n').filter(Boolean)) {
      const r = JSON.parse(line);
      if (!r.error) done.add(`${r.task}|${r.variant}|${r.repeat}`);
    }
  }
  const variants = args.variants.split(',').map((s) => s.trim()).filter(Boolean);
  const jobs = [];
  for (let rep = 1; rep <= Number(args.repeat); rep++) {
    // Loop jobs first: they are the longest, so the pool packs better.
    for (const vkey of [...variants].sort((x, y) => (VARIANTS[y].loop ? 1 : 0) - (VARIANTS[x].loop ? 1 : 0))) {
      for (const task of tasks) {
        if (!done.has(`${task.id}|${vkey}|${rep}`)) jobs.push({ task, vkey, rep });
      }
    }
  }
  const conc = Number(args.concurrency ?? provider.concurrency ?? 4);
  console.log(`${jobs.length} jobs (${done.size} already done), concurrency ${conc}, sandbox ${sandboxKindAvailable()}`);
  let next = 0;
  let finished = 0;
  const t0 = Date.now();
  async function worker() {
    while (next < jobs.length) {
      const { task, vkey, rep } = jobs[next++];
      const rec = await runJob(chat, task, vkey, rep);
      appendFileSync(jsonl, `${JSON.stringify(rec)}\n`);
      finished++;
      const mins = ((Date.now() - t0) / 60000).toFixed(1);
      console.log(`[${finished}/${jobs.length} ${mins}m] ${task.id} ${vkey}#${rep} ${rec.pass ? 'PASS' : 'fail'} ${(rec.wall_ms / 1000).toFixed(0)}s${rec.rounds ? ` rounds=${rec.rounds} ${rec.outcome}/${rec.stop}` : ''}${rec.error ? ` ERROR ${rec.error.split('\n')[0]}` : ''}`);
    }
  }
  await Promise.all(Array.from({ length: conc }, worker));

  const records = readFileSync(jsonl, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
  const meta = {
    provider: args.provider, model: provider.model, base_url: provider.base_url,
    thinking: provider.thinking, temperature: provider.temperature, date,
    variants: Object.fromEntries(variants.map((k) => [k, VARIANTS[k].name])),
    max_steps: Number(args['max-steps']), worker_steps: Number(args['worker-steps']),
    max_rounds: Number(args['max-rounds']), sandbox: sandboxKindAvailable(),
    tasks: tasks.map((t) => ({ id: t.id, group: t.group, hidden: t.hasHidden })),
  };
  writeFileSync(`${out}.json`, `${JSON.stringify({ meta, records }, null, 1)}\n`);
  writeFileSync(`${out}.md`, summarize({ meta, records }));
  console.log(`wrote ${out}.json and ${out}.md`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
