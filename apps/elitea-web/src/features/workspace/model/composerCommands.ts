/**
 * The workspace composer's local "/" commands. They act on this screen
 * only (nothing is sent to the agent); the page decides what each does.
 */
import { t } from '@/shared/i18n';

export type WorkspaceCommandId = 'new' | 'plan' | 'undo' | 'agent' | 'clear' | 'help';

export interface WorkspaceCommand {
  id: WorkspaceCommandId;
  /** As typed, with its "/". */
  name: string;
  description: string;
}

/** Every command, in menu order (descriptions in the current language). */
export function workspaceCommands(): WorkspaceCommand[] {
  return [
    { id: 'new', name: '/new', description: t('workspace.command.new', 'Start a new conversation') },
    { id: 'plan', name: '/plan', description: t('workspace.command.plan', 'Turn plan mode on or off') },
    { id: 'undo', name: '/undo', description: t('workspace.command.undo', 'Undo the files the last turn changed') },
    { id: 'agent', name: '/agent', description: t('workspace.command.agent', 'Choose the agent') },
    { id: 'clear', name: '/clear', description: t('workspace.command.clear', 'Clear the transcript on this screen') },
    { id: 'help', name: '/help', description: t('workspace.command.help', 'List these commands') },
  ];
}

/** The commands whose name starts with `/query` (case-insensitive). */
export function matchingCommands(query: string): WorkspaceCommand[] {
  const wanted = `/${query.toLowerCase()}`;
  return workspaceCommands().filter((command) => command.name.startsWith(wanted));
}
