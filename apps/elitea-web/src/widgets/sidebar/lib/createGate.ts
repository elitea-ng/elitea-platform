/**
 * Who may create what from the sidebar: the "+" of a row (New agent, New
 * pipeline, …) and the Chats caption's new chat, on the web and the desktop.
 * The rules are the ones the old split "Create" button applied (SHELL-013..026):
 *
 *  - each kind needs ANY of its create permissions (`CREATE_PERMISSIONS`);
 *    Artifacts needs none — anyone who can open the Artifacts page may create
 *    a bucket there (the old button's deliberate bypass);
 *  - in the public project, a person without a personal project creates
 *    nothing (the old `useDisablePersonalSpace` gate);
 *  - a new chat waits while one is being created.
 *
 * The public-project rule that hides a whole row (Skills) is
 * `visibleNavSections`'; a hidden row has no "+".
 */
import { getConfig } from '@/shared/config';
import { PERMISSIONS } from '@/shared/lib/permissions';

import type { NavItemValue } from './navSections';

export type CreatableNavItem = Exclude<NavItemValue, 'workspaces' | 'credentials'>;

/** `undefined` = no permission needed. */
const CREATE_PERMISSIONS: Readonly<Record<CreatableNavItem, readonly string[] | undefined>> = {
  chat: [PERMISSIONS.chat.folders.create, PERMISSIONS.chat.create],
  agents: [PERMISSIONS.applications.create],
  pipelines: [PERMISSIONS.applications.create],
  skills: [PERMISSIONS.applications.create],
  toolkits: [PERMISSIONS.toolkits.create],
  mcps: [PERMISSIONS.toolkits.create],
  applications: [PERMISSIONS.toolkits.create],
  artifacts: undefined,
};

/** A row with no create rule (Credentials, the desktop's Catalog and Help) creates nothing. */
export function isCreatable(value: string): value is CreatableNavItem {
  return Object.hasOwn(CREATE_PERMISSIONS, value);
}

function hasCreatePermission(value: CreatableNavItem, permissions: ReadonlySet<string>): boolean {
  const required = CREATE_PERMISSIONS[value];
  return required === undefined || required.some((permission) => permissions.has(permission));
}

/**
 * The public project with no personal project behind the person: nothing is
 * created there. An unresolved project (`undefined`) is not the public one.
 */
function isPersonalSpaceBlocked(projectId: string | undefined, personalProjectId: string | undefined): boolean {
  if (projectId === undefined || (personalProjectId !== undefined && personalProjectId !== '')) return false;
  const config = getConfig();
  return config.status === 'ok' && projectId === String(config.config.vite_public_project_id);
}

export interface CreateContext {
  readonly permissions: ReadonlySet<string>;
  readonly projectId: string | undefined;
  readonly personalProjectId: string | undefined;
  /** A new chat is being created right now. */
  readonly creatingChat: boolean;
}

export function canCreate(value: string, context: CreateContext): boolean {
  if (!isCreatable(value) || isPersonalSpaceBlocked(context.projectId, context.personalProjectId)) return false;
  if (value === 'chat' && context.creatingChat) return false;
  return hasCreatePermission(value, context.permissions);
}
