#!/usr/bin/env node
// Markdown summary of a results file.
//   node summarize.mjs results/qwen-2026-11-04.json > results/qwen-2026-11-04.md
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const NAMES = { a: '(a) single, thinking on', b: '(b) single, thinking off', c: '(c) single, auto', d: '(d) verified loop, auto', e: '(e) single, auto, told the check' };
const GROUPS = ['coding', 'extraction', 'doc'];

const mean = (xs) => (xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : 0);
const pct = (x) => `${(100 * x).toFixed(0)} %`;
const k = (x) => (x >= 10000 ? `${(x / 1000).toFixed(0)}k` : x >= 1000 ? `${(x / 1000).toFixed(1)}k` : x.toFixed(0));

// Keep the last record per task/variant/repeat.
export function dedupe(records) {
  const m = new Map();
  for (const r of records) m.set(`${r.task}|${r.variant}|${r.repeat}`, r);
  return [...m.values()];
}

// Errored runs (harness or server errors) are left out of the pass rate
// and the means, and are counted in their own column.
function row(label, all, { rounds = false } = {}) {
  const rs = all.filter((r) => !r.error);
  const errors = all.length - rs.length;
  const pass = mean(rs.map((r) => (r.pass ? 1 : 0)));
  const t = (f) => mean(rs.map((r) => f(r.tokens ?? { prompt: 0, completion: 0, reasoning: 0, calls: 0 })));
  const cells = [
    label,
    rs.length ? `${pct(pass)} (${rs.filter((r) => r.pass).length}/${rs.length})` : 'n/a',
    String(errors),
    k(t((x) => x.prompt)),
    k(t((x) => x.completion - x.reasoning)),
    k(t((x) => x.reasoning)),
    t((x) => x.calls).toFixed(1),
    `${mean(rs.map((r) => r.wall_ms / 1000)).toFixed(0)} s`,
  ];
  if (rounds) cells.push(rs.some((r) => r.rounds) ? mean(rs.filter((r) => r.rounds).map((r) => r.rounds)).toFixed(1) : '');
  return `| ${cells.join(' | ')} |`;
}

export function summarize({ meta, records }) {
  const recs = dedupe(records);
  const variants = Object.keys(meta.variants);
  const out = [];
  out.push(`# ADR-0038 phase 0 results: ${meta.provider} (${meta.date})`, '');
  out.push(`Model \`${meta.model}\` at ${meta.base_url}. Thinking control \`${meta.thinking?.kind}\`. Temperature ${meta.temperature}.`);
  const reps = Math.max(...recs.map((r) => r.repeat));
  out.push(`${meta.tasks.length} tasks, ${variants.length} variants, ${reps} repeat(s), ${recs.length} runs. Single run: at most ${meta.max_steps} steps. Loop: at most ${meta.max_rounds} rounds of at most ${meta.worker_steps} worker steps. Sandbox: ${meta.sandbox}.`, '');
  out.push('Tokens are means per run. Runs with a harness or server error are left out of the pass rate and the means and are counted under Errors. "Answer" is completion tokens without the reasoning tokens. Pass is the real check after the run (tests with the original test files, schema plus expected values, or the doc rubric).', '');

  const head = '| Variant | Pass | Errors | Prompt | Answer | Reasoning | Calls | Wall |';
  const sep = '|---|---|---|---|---|---|---|---|';
  out.push('## All tasks', '', `${head} Rounds |`, `${sep}---|`);
  for (const v of variants) out.push(row(NAMES[v] ?? v, recs.filter((r) => r.variant === v), { rounds: true }));
  out.push('');
  for (const g of GROUPS) {
    const gr = recs.filter((r) => r.group === g);
    if (!gr.length) continue;
    const label = g === 'doc' ? 'doc (loop is model-reviewed; pass = rubric)' : g;
    out.push(`## ${label}`, '', `${head} Rounds |`, `${sep}---|`);
    for (const v of variants) out.push(row(NAMES[v] ?? v, gr.filter((r) => r.variant === v), { rounds: true }));
    out.push('');
  }

  const hidden = recs.filter((r) => r.hidden_pass !== undefined && !r.error);
  if (hidden.length) {
    out.push('## Held-out tests (coding tasks with a hidden test file)', '', '| Variant | Public pass | Hidden pass | Public pass but hidden fail |', '|---|---|---|---|');
    for (const v of variants) {
      const rs = hidden.filter((r) => r.variant === v);
      out.push(`| ${NAMES[v] ?? v} | ${rs.filter((r) => r.public_pass).length}/${rs.length} | ${rs.filter((r) => r.hidden_pass).length}/${rs.length} | ${rs.filter((r) => r.public_pass && !r.hidden_pass).length} |`);
    }
    out.push('');
  }

  const loops = recs.filter((r) => r.rounds && !r.error);
  if (loops.length) {
    const count = (f) => loops.filter(f).length;
    const outcomes = {};
    const stops = {};
    for (const r of loops) {
      outcomes[r.outcome] = (outcomes[r.outcome] ?? 0) + 1;
      stops[r.stop] = (stops[r.stop] ?? 0) + 1;
    }
    out.push('## Loop behaviour', '');
    out.push(`- Outcomes: ${Object.entries(outcomes).map(([a, b]) => `${a} ${b}`).join(', ')}.`);
    out.push(`- Stop reasons: ${Object.entries(stops).map(([a, b]) => `${a} ${b}`).join(', ')}.`);
    out.push(`- Runs with a \`done\` overridden: ${count((r) => r.overrides > 0)}. Runs with a regression restore: ${count((r) => r.restores > 0)}. Runs with a cut-off worker: ${count((r) => r.cut_offs > 0)}. Flaky confirms: ${count((r) => r.flaky > 0)}.`);
    out.push(`- Loop says verified/model-reviewed but the real check fails: ${count((r) => (r.outcome === 'verified' || r.outcome === 'model_reviewed') && !r.pass)}. Loop says unverified but the real check passes: ${count((r) => r.outcome === 'unverified' && r.pass)}.`);
    const roles = {};
    for (const r of loops) for (const [role, x] of Object.entries(r.by_role ?? {})) {
      const a = (roles[role] ??= { prompt: 0, completion: 0, reasoning: 0, calls: 0 });
      a.prompt += x.prompt; a.completion += x.completion; a.reasoning += x.reasoning; a.calls += x.calls;
    }
    out.push('', '| Loop role | Calls/run | Prompt/run | Answer/run | Reasoning/run |', '|---|---|---|---|---|');
    for (const [role, a] of Object.entries(roles)) {
      const n = loops.length;
      out.push(`| ${role} | ${(a.calls / n).toFixed(1)} | ${k(a.prompt / n)} | ${k((a.completion - a.reasoning) / n)} | ${k(a.reasoning / n)} |`);
    }
    out.push('');
  }

  if (reps > 1) {
    out.push('## Noise between repeats', '', '| Variant | Tasks with the same result in every repeat |', '|---|---|');
    for (const v of variants) {
      const byTask = new Map();
      for (const r of recs.filter((x) => x.variant === v && !x.error)) {
        const a = byTask.get(r.task) ?? [];
        a.push(Boolean(r.pass));
        byTask.set(r.task, a);
      }
      const same = [...byTask.values()].filter((a) => a.every((x) => x === a[0])).length;
      out.push(`| ${NAMES[v] ?? v} | ${same}/${byTask.size} |`);
    }
    out.push('');
  }

  out.push('## Per task', '', `| Task | Group | ${variants.map((v) => `(${v})`).join(' | ')} |`, `|---|---|${variants.map(() => '---').join('|')}|`);
  for (const t of meta.tasks) {
    const cells = variants.map((v) => {
      const rs = recs.filter((r) => r.task === t.id && r.variant === v).sort((x, y) => x.repeat - y.repeat);
      return rs.map((r) => (r.error ? 'E' : r.pass ? (r.hidden_pass === false ? 'P*' : 'P') : '-')).join(' ');
    });
    out.push(`| ${t.id} | ${t.group} | ${cells.join(' | ')} |`);
  }
  out.push('', 'P = pass, - = fail, E = harness or server error, P* = public tests pass but the held-out tests fail. One mark per repeat.', '');
  const errors = recs.filter((r) => r.error);
  if (errors.length) out.push(`Errors: ${errors.length} runs. First: ${errors[0].error.split('\n')[0]}`, '');
  return out.join('\n');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const file = process.argv[2];
  if (!file) {
    console.error('usage: node summarize.mjs <results.json>');
    process.exit(2);
  }
  process.stdout.write(summarize(JSON.parse(readFileSync(file, 'utf8'))));
}
