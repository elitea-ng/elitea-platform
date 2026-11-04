// One agent run: a tool loop with a per-step thinking policy.
// Policy `auto` follows ADR-0038 decision 4: thinking on for the first step
// and for a step after a tool error or a failed check (a command that exited
// non-zero); off for routine follow-up steps.
import { TOOL_SPECS, execTool } from './sandbox.mjs';

export function thinkingFor(policy, { step, prevFailed }) {
  if (policy === 'on') return true;
  if (policy === 'off') return false;
  if (policy === 'auto') return step === 0 || prevFailed;
  throw new Error(`unknown policy ${policy}`);
}

export const WORKER_SYSTEM = [
  'You are an agent that works in a sandboxed workspace directory.',
  'Tools: read_file, write_file, list_files, run_command (shell in the workspace root; no network; 60 s limit).',
  'Paths are relative to the workspace root.',
  'Do the whole task. Check your work with the tools, for example run the tests or validate the output.',
  'When you are finished, reply with a short summary and do not call a tool.',
].join('\n');

// Returns {final, cutOff, cutReason, steps, messages, calls, toolErrors}.
export async function runAgent({ chat, run, system = WORKER_SYSTEM, user, policy, maxSteps = 40, role = 'agent', exec = execTool }) {
  const messages = [
    { role: 'system', content: system },
    { role: 'user', content: user },
  ];
  const calls = [];
  let prevFailed = false;
  let toolErrors = 0;
  for (let step = 0; step < maxSteps; step++) {
    const thinking = thinkingFor(policy, { step, prevFailed });
    const res = await chat(messages, { tools: TOOL_SPECS, thinking });
    calls.push({ role, step, thinking, ...res.usage, ms: res.ms });
    const msg = res.message ?? {};
    const toolCalls = Array.isArray(msg.tool_calls) ? msg.tool_calls : [];
    if (res.finish === 'length') {
      return { final: msg.content ?? '', cutOff: true, cutReason: 'length', steps: step + 1, messages, calls, toolErrors };
    }
    if (!toolCalls.length) {
      return { final: msg.content ?? '', cutOff: false, steps: step + 1, messages, calls, toolErrors };
    }
    messages.push({ role: 'assistant', content: msg.content ?? '', tool_calls: toolCalls });
    prevFailed = false;
    for (const tc of toolCalls) {
      const r = await exec(run, tc.function?.name, tc.function?.arguments);
      if (!r.ok) {
        prevFailed = true;
        toolErrors++;
      }
      messages.push({ role: 'tool', tool_call_id: tc.id, content: r.text });
    }
  }
  return { final: '', cutOff: true, cutReason: 'steps', steps: maxSteps, messages, calls, toolErrors };
}
