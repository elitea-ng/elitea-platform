/**
 * The tool envelope's lists as the Workforce tab edits them (issue 3137): one
 * entry per line. `tool_limits` is `toolkit/tool = limit`; a limit that is not
 * a number is kept as its text so the validation names it. An empty field is
 * the absent key (the platform's default), never an empty list.
 */
import { numberInput } from './agentMetaBlocks';

function lines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

export function parseToolList(text: string): string[] | undefined {
  const tools = lines(text);
  return tools.length === 0 ? undefined : tools;
}

export function formatToolList(value: unknown): string {
  return Array.isArray(value) ? value.filter((item) => typeof item === 'string').join('\n') : '';
}

export function parseToolLimits(text: string): Record<string, number | string> | undefined {
  const entries = lines(text).map((line): [string, number | string] => {
    const at = line.lastIndexOf('=');
    if (at < 0) return [line, ''];
    return [line.slice(0, at).trim(), numberInput(line.slice(at + 1)) ?? ''];
  });
  return entries.length === 0 ? undefined : Object.fromEntries(entries);
}

export function formatToolLimits(value: unknown): string {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return '';
  return Object.entries(value as Record<string, unknown>)
    .map(([key, limit]) => `${key} = ${String(limit)}`)
    .join('\n');
}
