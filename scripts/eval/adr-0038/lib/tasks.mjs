// Task loading, verifiers (ADR-0038 decision 7) and the final real check.
import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, existsSync, writeFileSync, mkdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { runCommand, overlay } from './sandbox.mjs';
import { validate, scoreChecks, scoreRubric } from './schema.mjs';
import { parseJsonReply } from './json.mjs';

export function loadTasks(tasksDir, filter) {
  const ids = readdirSync(tasksDir).filter((d) => existsSync(join(tasksDir, d, 'task.json'))).sort();
  const tasks = ids.map((id) => {
    const dir = join(tasksDir, id);
    const task = JSON.parse(readFileSync(join(dir, 'task.json'), 'utf8'));
    task.dir = dir;
    task.filesDir = join(dir, 'files');
    task.hiddenDir = join(dir, 'hidden');
    task.hasHidden = existsSync(task.hiddenDir);
    if (existsSync(join(dir, 'expected.json'))) task.expected = JSON.parse(readFileSync(join(dir, 'expected.json'), 'utf8'));
    if (existsSync(join(dir, 'rubric.json'))) task.rubric = JSON.parse(readFileSync(join(dir, 'rubric.json'), 'utf8'));
    return task;
  });
  if (!filter) return tasks;
  const re = new RegExp(filter);
  return tasks.filter((t) => re.test(t.id) || re.test(t.group));
}

const sha = (buf) => createHash('sha256').update(buf).digest('hex');

function hashFiles(ws, files) {
  const out = {};
  for (const f of files) {
    const p = join(ws, f);
    out[f] = existsSync(p) ? sha(readFileSync(p)) : null;
  }
  return out;
}

// Parse a pass/fail count from node --test or go test output, so that the
// loop can rank failing candidates.
export function testScore(output, code) {
  if (code === 0) return 1e6;
  const pass = output.match(/^[#ℹ]\s*pass (\d+)/m);
  const fail = output.match(/^[#ℹ]\s*fail (\d+)/m);
  if (pass && fail) return Number(pass[1]) - Number(fail[1]);
  const goFails = (output.match(/^\s*--- FAIL/gm) ?? []).length;
  if (/\[build failed\]|\[setup failed\]|syntax error|undefined:/.test(output)) return -1e5;
  return -goFails;
}

// Build the verifier for a task. `chat` is needed only by `review`.
export function makeVerifier(task, { chat, criteria = [] } = {}) {
  const v = task.verifier;
  if (v.kind === 'command') {
    const checkFiles = v.check_files ?? [];
    const pristine = hashFiles(task.filesDir, checkFiles);
    return {
      kind: 'command',
      strong: true,
      describe: () => `The command \`${v.command}\` must exit 0. These files define the check and must not change: ${checkFiles.join(', ')}.`,
      async run(run) {
        const r = await runCommand(run, v.command, { timeoutMs: 120_000, maxOutput: 3000 });
        const now = hashFiles(run.ws, checkFiles);
        const changed = checkFiles.filter((f) => now[f] !== pristine[f]);
        return {
          pass: r.code === 0 && changed.length === 0,
          rawPass: r.code === 0,
          changed,
          score: testScore(r.output, r.code),
          detail: (changed.length ? `verifier changed: ${changed.join(', ')}\n` : '') + `exit ${r.code}\n${r.output}`,
        };
      },
    };
  }
  if (v.kind === 'schema') {
    const schema = JSON.parse(readFileSync(join(task.filesDir, v.schema), 'utf8'));
    return {
      kind: 'schema',
      strong: true,
      describe: () => `${v.output} must exist, be valid JSON and validate against ${v.schema}.`,
      async run(run) {
        const p = join(run.ws, v.output);
        if (!existsSync(p)) return { pass: false, score: -1e5, detail: `${v.output} does not exist` };
        let data;
        try { data = JSON.parse(readFileSync(p, 'utf8')); } catch (err) {
          return { pass: false, score: -1e4, detail: `${v.output} is not valid JSON: ${err.message}` };
        }
        const errors = validate(schema, data);
        return {
          pass: errors.length === 0,
          score: -errors.length,
          detail: errors.length ? `schema errors (${errors.length}):\n${errors.slice(0, 20).join('\n')}` : 'valid against the schema',
        };
      },
    };
  }
  if (v.kind === 'review') {
    return {
      kind: 'review',
      strong: false,
      describe: () => `A separate reviewer model reads ${v.output} and checks it against the request and the acceptance criteria.`,
      async run(run, ctx = {}) {
        const p = join(run.ws, v.output);
        if (!existsSync(p)) return { pass: false, score: -1e5, detail: `${v.output} does not exist`, calls: [] };
        const doc = readFileSync(p, 'utf8').slice(0, 30_000);
        const crit = ctx.criteria ?? criteria;
        const messages = [
          { role: 'system', content: 'You are a strict reviewer. You check a document against a request and acceptance criteria. You have no tools. Reply with JSON only: {"pass": true|false, "issues": ["..."]}. Fail the document if any criterion is not met.' },
          { role: 'user', content: `# Request\n${task.prompt}\n\n# Acceptance criteria\n${crit.map((c) => `- ${c}`).join('\n') || '(none given)'}\n\n# Document ${v.output}\n${doc}` },
        ];
        const res = await chat(messages, { thinking: true, maxTokens: 6000 });
        const call = { role: 'reviewer', thinking: true, ...res.usage, ms: res.ms };
        const parsed = parseJsonReply(res.message.content);
        const pass = Boolean(parsed?.pass);
        const issues = Array.isArray(parsed?.issues) ? parsed.issues : [];
        return {
          pass,
          score: pass ? 1 : -issues.length,
          detail: pass ? 'reviewer: pass' : `reviewer: fail\n${issues.map((i) => `- ${i}`).join('\n') || '(no reasons given)'}`,
          calls: [call],
        };
      },
    };
  }
  throw new Error(`unknown verifier ${v.kind}`);
}

// The real check, run once after the attempt. Restores the pristine check
// files first, so editing the tests never helps.
export async function scoreFinal(task, run) {
  const v = task.verifier;
  if (task.group === 'coding') {
    const files = v.check_files ?? [];
    const before = hashFiles(task.filesDir, files);
    const after = hashFiles(run.ws, files);
    const tampered = files.filter((f) => before[f] !== after[f]);
    for (const f of files) {
      const dst = join(run.ws, f);
      mkdirSync(dirname(dst), { recursive: true });
      writeFileSync(dst, readFileSync(join(task.filesDir, f)));
    }
    const pub = await runCommand(run, v.command, { timeoutMs: 180_000, maxOutput: 2000 });
    const out = { pass: pub.code === 0, public_pass: pub.code === 0, tampered, detail: pub.output.slice(-800) };
    if (task.hasHidden) {
      overlay(task.hiddenDir, run.ws);
      const hid = await runCommand(run, task.final.hidden_command, { timeoutMs: 180_000, maxOutput: 2000 });
      out.hidden_pass = hid.code === 0;
      out.hidden_detail = hid.output.slice(-800);
    }
    return out;
  }
  if (task.group === 'extraction') {
    const schema = JSON.parse(readFileSync(join(task.filesDir, v.schema), 'utf8'));
    const p = join(run.ws, v.output);
    if (!existsSync(p)) return { pass: false, schema_valid: false, field_acc: 0, detail: 'no output' };
    let data;
    try { data = JSON.parse(readFileSync(p, 'utf8')); } catch {
      return { pass: false, schema_valid: false, field_acc: 0, detail: 'invalid JSON' };
    }
    const errors = validate(schema, data);
    const s = scoreChecks(task.expected.checks, data);
    return {
      pass: errors.length === 0 && s.failed.length === 0,
      schema_valid: errors.length === 0,
      field_acc: s.passed / s.total,
      detail: [...errors.slice(0, 5), ...s.failed.map((c) => `check ${c.op} ${c.path}`)].join('\n'),
    };
  }
  if (task.group === 'doc') {
    const p = join(run.ws, v.output);
    if (!existsSync(p)) return { pass: false, rubric_acc: 0, detail: 'no output' };
    const s = scoreRubric(task.rubric.checks, readFileSync(p, 'utf8'));
    return {
      pass: s.failed.length === 0,
      rubric_acc: s.passed / s.total,
      detail: s.failed.map((c) => c.desc ?? c.pattern ?? c.kind).join('\n'),
    };
  }
  throw new Error(`unknown group ${task.group}`);
}
