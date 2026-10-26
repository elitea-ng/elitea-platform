/**
 * The local agent (ADR-0037 decision 1): an `AGENTS.md` file in a desktop
 * workspace, written by the session agent with the existing local write tools
 * behind the approval card, and read by the unchanged D0 loader
 * (`libs/rust/local-tools/src/project_instructions.rs`).
 *
 * The loader is the authority. These checks mirror `LocalAgentDefinition`
 * (`services/elitea-main/api/openapi/v2.yaml`) so the builder can tell a
 * person before the write that a file would be cut or refused; they decide
 * nothing the loader does not decide again on every read.
 */

/** The loader's file name and its cap on all AGENTS.md text one turn reads (`FILE_NAME`, `MAX_TOTAL_BYTES`). */
export const AGENTS_FILE = 'AGENTS.md';
export const AGENTS_MAX_TOTAL_BYTES = 32 * 1024;

/** `LocalAgentDefinition`'s length bounds. */
export const LOCAL_AGENT_NAME_MAX = 64;
export const LOCAL_AGENT_DESCRIPTION_MAX = 2304;
export const LOCAL_AGENT_SCOPE_MAX = 512;

/** `LocalAgentDefinition`: what the person asks for. */
export interface LocalAgentDefinition {
  readonly name: string;
  readonly description?: string;
  readonly instructions: string;
  /** `root`, or a folder relative to the workspace (`apps/web`). */
  readonly scope: string;
}

export type LocalAgentIssue = 'name_missing' | 'name_too_long' | 'description_too_long' | 'instructions_missing' | 'too_large' | 'scope_invalid';

// The schema's `scope` pattern: `root`, or `/`-separated segments that start
// with a letter, a digit or `_` (so no leading `/`, no `.` or `..` segment).
const SCOPE_PATTERN = /^(root|[A-Za-z0-9_][A-Za-z0-9_.@ -]*(\/[A-Za-z0-9_][A-Za-z0-9_.@ -]*)*)$/u;

/** The UTF-8 size of `text`, as the loader counts it. */
export function utf8Bytes(text: string): number {
  return new TextEncoder().encode(text).length;
}

/** A scope as typed (`./apps/web/`, ` `) in its canonical form (`apps/web`, `root`). */
export function normalizeScope(scope: string): string {
  const trimmed = scope.trim().replace(/^\.\/+/u, '').replace(/\/+$/u, '');
  return trimmed === '' || trimmed === '.' ? 'root' : trimmed;
}

/** The file the definition is written to, relative to the workspace root. */
export function localAgentPath(scope: string): string {
  const normalized = normalizeScope(scope);
  return normalized === 'root' ? AGENTS_FILE : `${normalized}/${AGENTS_FILE}`;
}

/** The file's text: a title, the description as a lead paragraph, then the instructions. */
export function renderAgentsMd(definition: LocalAgentDefinition): string {
  const parts = [`# ${definition.name.trim()}`];
  const description = definition.description?.trim() ?? '';
  if (description !== '') parts.push(description);
  parts.push(definition.instructions.trim());
  return `${parts.join('\n\n')}\n`;
}

/** Whether `scope` (as typed) names `root` or a folder the schema admits. */
export function isValidScope(scope: string): boolean {
  const normalized = normalizeScope(scope);
  return normalized.length <= LOCAL_AGENT_SCOPE_MAX && SCOPE_PATTERN.test(normalized);
}

/** What is wrong with a definition, empty when it can be written. */
export function validateLocalAgent(definition: LocalAgentDefinition): LocalAgentIssue[] {
  const issues: LocalAgentIssue[] = [];
  const name = definition.name.trim();
  if (name === '') issues.push('name_missing');
  if ([...name].length > LOCAL_AGENT_NAME_MAX) issues.push('name_too_long');
  if ([...(definition.description ?? '')].length > LOCAL_AGENT_DESCRIPTION_MAX) issues.push('description_too_long');
  if (definition.instructions.trim() === '') issues.push('instructions_missing');
  if (utf8Bytes(renderAgentsMd(definition)) > AGENTS_MAX_TOTAL_BYTES) issues.push('too_large');
  if (!isValidScope(definition.scope)) issues.push('scope_invalid');
  return issues;
}

export interface LocalAgentRequest {
  /** What the agent should do, in the person's words. */
  readonly request: string;
  /** `root` or a folder; the file goes there. */
  readonly scope: string;
  /** A name the person chose, if any. */
  readonly name?: string;
}

/**
 * The message the desktop sends into the folder session to create a local
 * agent. It is an ordinary user message: the transcript shows it, and the
 * write goes through `write_file`/`edit_file` and the approval card like any
 * other edit. There is no new tool and no new host command.
 */
export function localAgentPrompt(input: LocalAgentRequest): string {
  const path = localAgentPath(input.scope);
  const nested = path !== AGENTS_FILE;
  const lines = [
    `Create a local agent for this folder: write its instructions to \`${path}\`.`,
    '',
    `What the agent should do: ${input.request.trim()}`,
    ...(input.name !== undefined && input.name.trim() !== '' ? [`Name: ${input.name.trim()}`] : []),
    '',
    'Rules for the file:',
    `- Start with a "# <name>" heading (at most ${String(LOCAL_AGENT_NAME_MAX)} characters), then one or two sentences on what the agent is for, then the instructions in Markdown, addressed to the agent.`,
    `- Keep it under ${String(AGENTS_MAX_TOTAL_BYTES)} bytes. Every ${AGENTS_FILE} a turn reads counts against that one limit, so be brief.`,
    `- If \`${path}\` exists, read it first and merge the new instructions into it with edit_file rather than replacing what still applies.`,
    '- Look at the folder first (its README, build files and layout) so the instructions name the real commands and conventions. Do not invent any.',
    '- Do not write the file through a symlink, and do not change any other file.',
    ...(nested ? [`- This file applies when a turn works under \`${normalizeScope(input.scope)}/\`, in addition to the root ${AGENTS_FILE}.`] : []),
    '',
    'When the file is written, say in one line what the agent will do on the next turn.',
  ];
  return lines.join('\n');
}
