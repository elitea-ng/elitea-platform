/**
 * The add/edit form of one MCP server, as text fields, and its checks
 * against the host's `mcp.json` schema (IPC.md, "Local MCP servers"): the
 * same bounds the host enforces, so the form says what is wrong before a
 * save is refused.
 */
import type { McpScope, McpServerConfig } from '@/shared/desktop/mcpIpc';

export interface ServerForm {
  name: string;
  scope: McpScope;
  command: string;
  /** One argument per line. */
  args: string;
  /** One `KEY=value` per line. */
  env: string;
  cwd: 'workspace' | 'none';
  network: boolean;
  enabled: boolean;
  /** Comma-separated; empty: every tool the server lists. */
  allowedTools: string;
  /** Comma-separated. */
  readOnlyTools: string;
  trustReadOnlyHints: boolean;
}

type FormField = 'name' | 'command' | 'args' | 'env';
/** What is wrong with a field (the panel words it); `key` names the variable. */
export type FormProblem =
  | { code: 'name' | 'command' | 'argsCount' | 'argLength' | 'envCount' }
  | { code: 'envKey' | 'envValue'; key: string };
export type FormErrors = Partial<Record<FormField, FormProblem>>;

const NAME = /^[A-Za-z0-9_-]{1,32}$/;
const ENV_KEY = /^[A-Z_][A-Z0-9_]*$/;
const MAX_ITEMS = 64;
const MAX_TEXT = 4096;

export const EMPTY_FORM: ServerForm = {
  name: '',
  scope: 'user',
  command: '',
  args: '',
  env: '',
  cwd: 'workspace',
  network: false,
  enabled: true,
  allowedTools: '',
  readOnlyTools: '',
  trustReadOnlyHints: false,
};

const lines = (text: string): string[] => text.split('\n').map((line) => line.trim()).filter((line) => line !== '');
const list = (text: string): string[] => text.split(',').map((item) => item.trim()).filter((item) => item !== '');

/** A stored server as the form edits it. */
export function formOf(name: string, scope: McpScope, server: McpServerConfig): ServerForm {
  return {
    name,
    scope,
    command: server.command,
    args: (server.args ?? []).join('\n'),
    env: Object.entries(server.env ?? {})
      .map(([key, value]) => `${key}=${value}`)
      .join('\n'),
    cwd: server.cwd ?? 'workspace',
    network: server.network ?? false,
    enabled: server.enabled ?? true,
    allowedTools: (server.allowed_tools ?? []).join(', '),
    readOnlyTools: (server.read_only_tools ?? []).join(', '),
    trustReadOnlyHints: server.trust_read_only_hints ?? false,
  };
}

function envOf(text: string): { env: Record<string, string>; error?: FormProblem } {
  const env: Record<string, string> = {};
  for (const line of lines(text)) {
    const at = line.indexOf('=');
    const key = at < 0 ? line : line.slice(0, at);
    if (!ENV_KEY.test(key)) return { env, error: { code: 'envKey', key } };
    const value = at < 0 ? '' : line.slice(at + 1);
    if (value.length > MAX_TEXT) return { env, error: { code: 'envValue', key } };
    env[key] = value;
  }
  if (Object.keys(env).length > MAX_ITEMS) return { env, error: { code: 'envCount' } };
  return { env };
}

function argsError(args: string[]): FormProblem | undefined {
  if (args.length > MAX_ITEMS) return { code: 'argsCount' };
  return args.some((arg) => arg.length > MAX_TEXT) ? { code: 'argLength' } : undefined;
}

/** The server the form describes, or what is wrong with it, field by field. */
export function checkForm(form: ServerForm): { server: McpServerConfig; errors: FormErrors } {
  const errors: FormErrors = {};
  if (!NAME.test(form.name)) errors.name = { code: 'name' };
  const command = form.command.trim();
  if (!command.startsWith('/')) errors.command = { code: 'command' };
  const args = lines(form.args);
  const argsProblem = argsError(args);
  if (argsProblem !== undefined) errors.args = argsProblem;
  const { env, error: envProblem } = envOf(form.env);
  if (envProblem !== undefined) errors.env = envProblem;
  const allowed = list(form.allowedTools);
  const server: McpServerConfig = {
    command,
    args,
    env,
    cwd: form.cwd,
    network: form.network,
    enabled: form.enabled,
    allowed_tools: allowed.length === 0 ? null : allowed,
    read_only_tools: list(form.readOnlyTools),
    trust_read_only_hints: form.trustReadOnlyHints,
  };
  return { server, errors };
}
