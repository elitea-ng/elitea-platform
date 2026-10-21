/**
 * The workspace composer's "/" menu, as pure functions.
 *
 * - The host commands (`/new`, `/plan`, …) act on this screen only
 *   (nothing is sent to the agent); the page decides what each does.
 * - The custom commands (`commands_list`: the workspace's `.elitea/commands`
 *   and `.claude/commands`, then the person's own folder) are prompt
 *   templates: picking one inserts `/<name> `, and a message that starts
 *   with it is sent as that command, which the host reads and expands.
 * - The agent's skills (`composerSkills.ts`) come last.
 *
 * A name is offered once: a host command hides a custom command of its
 * name, and a custom command hides a skill of its name (the message would
 * run the command).
 */
import type { CustomCommand, TurnCommand } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { matchingSkills, type ComposerSkill } from './composerSkills';

export type WorkspaceCommandId = 'new' | 'plan' | 'undo' | 'agent' | 'model' | 'clear' | 'compact' | 'help';

export interface WorkspaceCommand {
  id: WorkspaceCommandId;
  /** As typed, with its "/". */
  name: string;
  description: string;
}

/** Every command, in menu order (descriptions in the current language). */
function workspaceCommands(): WorkspaceCommand[] {
  return [
    { id: 'new', name: '/new', description: t('workspace.command.new', 'Start a new conversation') },
    { id: 'plan', name: '/plan', description: t('workspace.command.plan', 'Turn plan mode on or off') },
    { id: 'undo', name: '/undo', description: t('workspace.command.undo', 'Undo the files the last turn changed') },
    { id: 'agent', name: '/agent', description: t('workspace.command.agent', 'Choose the agent') },
    { id: 'model', name: '/model', description: t('workspace.command.model', 'Choose the model (/model <name> sets it)') },
    { id: 'clear', name: '/clear', description: t('workspace.command.clear', 'Clear the transcript on this screen') },
    { id: 'compact', name: '/compact', description: t('workspace.command.compact', 'Summarize the conversation so far at the next message') },
    { id: 'help', name: '/help', description: t('workspace.command.help', 'List these commands') },
  ];
}

/** The commands whose name starts with `/query` (case-insensitive). */
export function matchingCommands(query: string): WorkspaceCommand[] {
  const wanted = `/${query.toLowerCase()}`;
  return workspaceCommands().filter((command) => command.name.startsWith(wanted));
}

/**
 * `/model <name>` as the whole message: the model it names (the host checks
 * it against the project's chat models at the next send), else null.
 */
export function modelCommandName(text: string): string | null {
  const match = /^\/model\s+(\S+)\s*$/u.exec(text.trim());
  return match?.[1] ?? null;
}

/** How many custom commands the menu lists at most. */
const CUSTOM_LIMIT = 50;

/** The host commands' names (without "/"): a custom command of one of them is not offered. */
const HOST_NAMES = new Set<string>(['new', 'plan', 'undo', 'agent', 'model', 'clear', 'compact', 'help'] satisfies WorkspaceCommandId[]);

/** The custom commands the menu offers: none under a host command's name. */
function offered(commands: readonly CustomCommand[]): CustomCommand[] {
  return commands.filter((command) => !HOST_NAMES.has(command.name));
}

/** What the composer inserts for a picked custom command. */
export function customCommandToken(command: CustomCommand): string {
  return `/${command.name}`;
}

/**
 * The custom commands whose name holds `query` (case-insensitive): the
 * workspace's before the person's, names starting with it first, then the
 * rest, each alphabetically; at most 50.
 */
export function matchingCustomCommands(commands: readonly CustomCommand[], query: string): CustomCommand[] {
  const wanted = query.toLowerCase();
  const rank = (command: CustomCommand): number => (command.source === 'workspace' ? 0 : 2) + (command.name.startsWith(wanted) ? 0 : 1);
  return offered(commands)
    .filter((command) => command.name.includes(wanted))
    .sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name))
    .slice(0, CUSTOM_LIMIT);
}

/** The source badge of a custom command: where its file is. */
export function customCommandSource(command: CustomCommand): string {
  return command.source === 'workspace' ? t('workspace.command.sourceWorkspace', 'workspace') : t('workspace.command.sourceUser', 'yours');
}

/**
 * A custom command's detail line: its description (else its file), its
 * argument hint, and what the host ignored in its file.
 */
export function customCommandDetail(command: CustomCommand): string {
  const parts = [command.description ?? command.path];
  if (command.argument_hint !== null) parts.push(command.argument_hint);
  if (command.ignored.length > 0) parts.push(t('workspace.command.ignored', 'ignored: {{keys}}', { keys: command.ignored.join(', ') }));
  return parts.join(' · ');
}

/**
 * The custom command `text` runs: the message (leading blanks aside)
 * starts with `/<name>` followed by whitespace or its end; the arguments
 * are the rest. `null` when it runs none.
 */
export function invokedCustomCommand(text: string, commands: readonly CustomCommand[]): TurnCommand | null {
  const message = text.trimStart();
  let best: CustomCommand | null = null;
  for (const command of offered(commands)) {
    const token = customCommandToken(command);
    const next = message.charAt(token.length);
    if (message.startsWith(token) && (next === '' || /\s/u.test(next)) && (best === null || command.name.length > best.name.length)) best = command;
  }
  return best === null ? null : { name: best.name, arguments: message.slice(customCommandToken(best).length).trim() };
}

/** One row of the "/" menu. */
export type SlashEntry =
  | { kind: 'host'; command: WorkspaceCommand }
  | { kind: 'custom'; command: CustomCommand }
  | { kind: 'skill'; skill: ComposerSkill };

/**
 * The "/" menu for `query`: the host commands, then the custom commands
 * (the workspace's, then the person's), then the skills no custom command
 * hides.
 */
export function slashMenu(query: string, commands: readonly CustomCommand[], skills: readonly ComposerSkill[]): SlashEntry[] {
  const custom = matchingCustomCommands(commands, query);
  const hidden = new Set(offered(commands).map((command) => command.name));
  return [
    ...matchingCommands(query).map((command): SlashEntry => ({ kind: 'host', command })),
    ...custom.map((command): SlashEntry => ({ kind: 'custom', command })),
    ...matchingSkills(skills, query)
      .filter((skill) => !hidden.has(skill.name.toLowerCase()))
      .map((skill): SlashEntry => ({ kind: 'skill', skill })),
  ];
}
