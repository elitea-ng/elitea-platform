/**
 * The hooks editor's text (#1237): a file's hooks as pretty JSON, and the
 * person's edit read back with the checks that catch a typo before the
 * host's own schema check (which stays the authority: it names the hook it
 * refuses).
 */
import type { Hook } from '@/shared/desktop/hooksIpc';

const EVENTS = new Set(['PreToolUse', 'PostToolUse', 'Stop', 'UserPromptSubmit']);

export function hooksText(hooks: readonly Hook[]): string {
  return JSON.stringify(hooks, null, 2);
}

export type ParsedHooks = { ok: true; hooks: Hook[] } | { ok: false; problem: string };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** What is wrong with the `index`th entry, if anything (its id noted in `ids`). */
function entryProblem(entry: unknown, index: number, ids: Set<string>): string | null {
  const where = `hook ${index + 1}`;
  if (!isRecord(entry)) return `${where} is not an object`;
  const { id, event, command } = entry;
  if (typeof id !== 'string' || !/^[A-Za-z0-9_-]{1,64}$/.test(id)) return `${where}: id is [A-Za-z0-9_-], 1 to 64 characters`;
  if (ids.has(id)) return `${where}: the id ${id} is used twice`;
  ids.add(id);
  if (typeof event !== 'string' || !EVENTS.has(event)) return `${id}: event is PreToolUse, PostToolUse, Stop or UserPromptSubmit`;
  const argv = Array.isArray(command) && command.length > 0 && command.every((item) => typeof item === 'string');
  return argv ? null : `${id}: command is a non-empty array of strings`;
}

/** The edited text as hooks, or the first problem found. */
export function parseHooks(text: string): ParsedHooks {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    return { ok: false, problem: `not JSON: ${error instanceof Error ? error.message : String(error)}` };
  }
  if (!Array.isArray(value)) return { ok: false, problem: 'the hooks are a JSON array' };
  const ids = new Set<string>();
  for (const [index, entry] of value.entries()) {
    const problem = entryProblem(entry, index, ids);
    if (problem !== null) return { ok: false, problem };
  }
  return { ok: true, hooks: value as Hook[] };
}
