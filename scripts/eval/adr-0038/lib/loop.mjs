// Prototype verified loop (ADR-0038 decisions 6-8), kept in the harness.
//
// Plan (manager, thinking on) -> rounds of: fresh-context worker (auto
// policy) -> verifier -> manager review (thinking on). Hard rules are code:
//  - no `done` while a strong verifier fails (overridden to continue);
//  - regression guard: a passing result is never replaced by a failing one;
//  - stop at the round limit (default 8) or when the manager issues the same
//    task twice in a row;
//  - a cut-off worker gets a summary call (thinking off);
//  - a strong pass is confirmed once by a re-run (flaky otherwise);
//  - the check files are hashed at the start; a pass on changed check files
//    counts as "verifier changed", not as a pass.
import { listFiles, snapshot, restore } from './sandbox.mjs';
import { runAgent, WORKER_SYSTEM } from './agent.mjs';
import { parseJsonReply } from './json.mjs';

const MANAGER_SYSTEM = [
  'You are the manager of a verified loop.',
  'You plan and review. Workers do the work in fresh contexts. A worker sees only the ledger and the workspace, never the earlier transcripts.',
  'A program check (the verifier) runs after every round. Its verdict is ground truth.',
  'Reply with one JSON object only.',
].join('\n');

const normTask = (s) => String(s ?? '').toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();

export function renderLedger(ledger, { verifier, maxRounds }) {
  const lines = [];
  lines.push('# Goal', ledger.goal, '');
  lines.push('# Acceptance criteria', ...(ledger.criteria.length ? ledger.criteria.map((c) => `- ${c}`) : ['- (none)']), '');
  lines.push('# The check', verifier.describe(), '');
  if (ledger.tasks.length) lines.push('# Task list (manager)', ...ledger.tasks.map((t, i) => `${i + 1}. ${t}`), '');
  if (ledger.rounds.length) {
    lines.push(`# Rounds so far (limit ${maxRounds})`);
    for (const r of ledger.rounds) {
      lines.push(`## Round ${r.round}: ${r.task}`, `Verdict: ${r.pass ? 'PASS' : 'FAIL'}${r.note ? ` (${r.note})` : ''}`, r.detail.slice(0, 1200), '');
    }
  }
  if (ledger.notes) lines.push('# Notes from the last worker', ledger.notes, '');
  return lines.join('\n');
}

async function managerCall(chat, user, calls, role) {
  const messages = [
    { role: 'system', content: MANAGER_SYSTEM },
    { role: 'user', content: user },
  ];
  for (let attempt = 0; attempt < 2; attempt++) {
    const res = await chat(messages, { thinking: true });
    calls.push({ role, thinking: true, ...res.usage, ms: res.ms });
    const parsed = parseJsonReply(res.message?.content);
    if (parsed) return parsed;
    messages.push({ role: 'assistant', content: res.message?.content ?? '' });
    messages.push({ role: 'user', content: 'That was not valid JSON. Reply with the JSON object only.' });
  }
  return null;
}

export async function runVerifiedLoop({
  chat, run, task, verifier, maxRounds = 8, workerSteps = 20, exec, confirm = true,
}) {
  const calls = [];
  const ledger = { goal: task.prompt, criteria: [], tasks: [], notes: '', rounds: [] };
  const events = [];
  const opts = { verifier, maxRounds };

  // Baseline verdict for strong verifiers: facts for the plan, no model cost.
  let baseline = null;
  if (verifier.strong) baseline = await verifier.run(run);

  const files = listFiles(run.ws).slice(0, 80).join('\n');
  const plan = await managerCall(chat, [
    renderLedger(ledger, opts),
    '# Workspace files', files, '',
    baseline ? `# Check result before any work\n${baseline.pass ? 'PASS' : 'FAIL'}\n${baseline.detail.slice(0, 2000)}\n` : '',
    'Write the plan. Reply with JSON:',
    '{"acceptance_criteria": ["..."], "tasks": ["..."], "first_task": "..."}',
    'Each task is a self-contained instruction that one worker can finish in one round (at most about 20 tool steps).',
    'Prefer few, larger tasks. The whole goal may be one task.',
  ].join('\n'), calls, 'manager');
  ledger.criteria = Array.isArray(plan?.acceptance_criteria) ? plan.acceptance_criteria.map(String) : [];
  ledger.tasks = Array.isArray(plan?.tasks) ? plan.tasks.map(String) : [];
  let current = String(plan?.first_task ?? ledger.tasks[0] ?? task.prompt);

  let best = null; // {dir, pass, score, round}
  let last = null;
  let stop = 'round_limit';
  let overrides = 0;
  let cutOffs = 0;
  let restores = 0;
  let flaky = 0;
  let verifierChanged = false;
  let round = 0;

  while (round < maxRounds) {
    round++;
    const user = [
      renderLedger(ledger, opts),
      '# Your task for this round',
      current,
      '',
      'The workspace holds the current result. Work on it with the tools.',
      'When you are done, reply with a short report (at most 200 words): what you changed, what you checked, and what remains. The report becomes the notes for the next worker.',
    ].join('\n');
    const w = await runAgent({ chat, run, system: WORKER_SYSTEM, user, policy: 'auto', maxSteps: workerSteps, role: 'worker', exec });
    calls.push(...w.calls);
    let report = w.final;
    if (w.cutOff) {
      cutOffs++;
      const msgs = [...w.messages, { role: 'user', content: 'You were cut off. Summarise in at most 150 words what you changed and what remains. Do not call a tool.' }];
      const res = await chat(msgs, { thinking: false, maxTokens: 800 });
      calls.push({ role: 'summary', thinking: false, ...res.usage, ms: res.ms });
      report = res.message?.content ?? '';
    }
    ledger.notes = String(report ?? '').slice(0, 8192);

    const verdict = await verifier.run(run, { criteria: ledger.criteria });
    if (verdict.calls) calls.push(...verdict.calls);
    let note = '';
    if (verdict.changed?.length && verdict.rawPass) {
      verifierChanged = true;
      note = 'verifier changed';
    }
    // The confirm re-run is for command verifiers only: a schema check on the
    // same file cannot change, and a second review call is a new verdict.
    if (verdict.pass && verifier.kind === 'command' && confirm) {
      const again = await verifier.run(run, { criteria: ledger.criteria });
      if (!again.pass) {
        verdict.pass = false;
        flaky++;
        note = 'flaky: the confirm re-run failed';
      }
    }
    // Regression guard. A result with changed check files ranks lowest: its
    // score comes from a check it altered, so it never becomes the best.
    const tampered = Boolean(verdict.changed?.length);
    if (verdict.pass) {
      best = { dir: snapshot(run), pass: true, score: verdict.score, round };
    } else if (best?.pass) {
      restore(run, best.dir);
      restores++;
      note = `${note ? `${note}; ` : ''}regression: restored the passing result of round ${best.round}`;
    } else if (!tampered && (!best || verdict.score >= best.score)) {
      best = { dir: snapshot(run), pass: false, score: verdict.score, round };
    }
    last = verdict;
    ledger.rounds.push({ round, task: current, pass: verdict.pass, note, detail: verdict.detail ?? '' });
    events.push({ round, task: current, pass: verdict.pass, note, steps: w.steps, cutOff: w.cutOff });

    if (round >= maxRounds) { stop = 'round_limit'; break; }

    const spent = calls.reduce((a, c) => a + c.prompt + c.completion, 0);
    const status = `Status: round ${round} of ${maxRounds} used; tokens spent ${spent}; verdicts ${ledger.rounds.map((r) => (r.pass ? 'PASS' : 'FAIL')).join(', ')}.`;
    const review = await managerCall(chat, [
      renderLedger(ledger, opts),
      'Review the round. The verdict is ground truth.',
      'Say "done" only when the check passes and the acceptance criteria are met. Otherwise give the next task.',
      'If rounds stop making progress, switch approach. Issuing the same task twice in a row stops the run.',
      'Reply with JSON: {"decision": "done" | "continue", "next_task": "...", "reason": "..."}',
      '',
      status,
    ].join('\n'), calls, 'manager');
    let decision = review?.decision === 'done' ? 'done' : 'continue';
    let next = String(review?.next_task ?? '').trim();
    const passNow = best?.pass === true;
    if (decision === 'done' && verifier.strong && !passNow) {
      overrides++;
      decision = 'continue';
      if (!next) next = `The check still fails. Fix it:\n${(last.detail ?? '').slice(0, 800)}`;
    }
    if (decision === 'done') { stop = 'done'; break; }
    if (!next) next = `Continue the goal. The last verdict:\n${(last.detail ?? '').slice(0, 800)}`;
    if (normTask(next) === normTask(current)) { stop = 'same_task'; break; }
    current = next;
  }

  // Return the best candidate.
  if (best && (best.pass || !last?.pass)) restore(run, best.dir);
  const passed = best?.pass === true;
  let outcome;
  if (passed) outcome = verifier.strong ? 'verified' : 'model_reviewed';
  else if (verifierChanged) outcome = 'verifier_changed';
  else if (flaky) outcome = 'flaky';
  else outcome = 'unverified';

  // Finish: the final answer from the ledger (thinking off).
  const fin = await chat([
    { role: 'system', content: 'Write the final answer for the user from the ledger. At most 120 words.' },
    { role: 'user', content: `${renderLedger(ledger, opts)}\nOutcome: ${outcome}.` },
  ], { thinking: false, maxTokens: 600 });
  calls.push({ role: 'finish', thinking: false, ...fin.usage, ms: fin.ms });

  return { outcome, stop, rounds: round, overrides, cutOffs, restores, flaky, verifierChanged, calls, events, criteria: ledger.criteria };
}
