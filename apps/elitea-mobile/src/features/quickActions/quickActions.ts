import * as QuickActions from 'expo-quick-actions';
import { Platform } from 'react-native';

import { log } from '@/lib/log';
import { registerWipeHook } from '@/security/endSession';

/**
 * Home-screen quick actions (long-press the app icon), for the ACTIVE
 * workspace: "New chat", "Notifications" (with the unread COUNT), and the
 * most recent chats by name only when the workspace policy allows titles
 * outside the app (`allow_system_surfaces`, client contract 1.3; off by
 * default, so by default no conversation name reaches the home screen).
 *
 * An action only opens a screen of the app; the device lock still covers it
 * (the lock gate sits above every route). Each action names the workspace it
 * was built for, and one built for another workspace opens nothing; a chat
 * also names its project and opens nothing in another one. The
 * items are cleared when a workspace is wiped (log out, revocation; before
 * the server revoke), at a launch that restores no signed-in workspace (a
 * wipe resumed after a kill), and rebuilt for whichever workspace is active
 * next.
 *
 * Conversation titles are iOS only. On Android a launcher can PIN a dynamic
 * shortcut to the home screen, and setItems (setDynamicShortcuts) never
 * removes, disables or relabels a pinned one, so a pinned title would outlive
 * the wipe, a revocation and the policy turning titles off. Android gets the
 * two fixed actions until the clear path also disables pinned chat shortcuts
 * (ShortcutManager.disableShortcuts; zefir-ca/agent-zefir#35).
 */

export const MAX_RECENT_CHATS = 2;
const MAX_TITLE = 60;

export interface RecentChat {
  /** The thread route reference (numeric id or uuid). */
  ref: string;
  name: string;
}

export interface QuickActionInput {
  workspaceId: string;
  /** The project the recent chats were listed in (conversation ids are per project). */
  projectId?: number | null;
  allowTitles: boolean;
  unread: number;
  recent: readonly RecentChat[];
}

export type ZefirAction = QuickActions.Action & { params: { href: string; workspace: string; project?: string } };

function cleanTitle(name: string): string | null {
  const one = name.replace(/\s+/g, ' ').trim();
  if (one === '') return null;
  return one.length > MAX_TITLE ? `${one.slice(0, MAX_TITLE - 1)}…` : one;
}

export function buildQuickActions(input: QuickActionInput): ZefirAction[] {
  const workspace = input.workspaceId;
  const unread = Math.max(0, Math.floor(input.unread));
  const actions: ZefirAction[] = [
    { id: 'new-chat', title: 'New chat', icon: 'compose', params: { href: '/new', workspace } },
    {
      id: 'notifications',
      title: 'Notifications',
      subtitle: unread > 0 ? `${unread > 99 ? '99+' : unread} unread` : null,
      icon: 'symbol:bell',
      params: { href: '/notifications', workspace },
    },
  ];
  const project = typeof input.projectId === 'number' ? String(input.projectId) : null;
  if (!input.allowTitles || !titlesCanBeCleared() || project === null) return actions;
  for (const chat of input.recent) {
    if (actions.length >= 2 + MAX_RECENT_CHATS) break;
    const title = cleanTitle(chat.name);
    if (title === null || !/^[0-9A-Za-z-]{1,64}$/.test(chat.ref)) continue;
    actions.push({ id: `chat:${chat.ref}`, title, icon: 'message', params: { href: `/c/${chat.ref}`, workspace, project } });
  }
  return actions;
}

function titlesCanBeCleared(): boolean {
  return Platform.OS === 'ios';
}

/**
 * True when an action may run now: it was built for the workspace that is
 * active and, for a chat, for the selected project too. Conversation ids are
 * per project (each project has its own tenant schema), so a chat built for
 * another project would open a 404 or a different conversation with the same
 * id (F2). A chat action naming no project is refused.
 */
export function actionBelongsTo(action: QuickActions.Action, workspaceId: string | null, projectId: number | null = null): boolean {
  const owner = action.params?.workspace;
  if (workspaceId === null || typeof owner !== 'string' || owner !== workspaceId) return false;
  const href = action.params?.href;
  if (typeof href === 'string' && href.startsWith('/c/')) {
    const project = action.params?.project;
    return projectId !== null && typeof project === 'string' && project === String(projectId);
  }
  return true;
}

let lastKey: string | null = null;

/** Replaces the items (skipped when nothing changed). Never throws. */
export async function applyQuickActions(actions: readonly QuickActions.Action[]): Promise<void> {
  const key = JSON.stringify(actions);
  if (key === lastKey) return;
  lastKey = key;
  try {
    if (!(await QuickActions.isSupported())) return;
    await QuickActions.setItems([...actions]);
  } catch (error) {
    lastKey = null;
    log.warn('quick actions not updated', { error });
  }
}

export function clearQuickActions(): Promise<void> {
  return applyQuickActions([]);
}

/**
 * Launch-time clear when no workspace was restored signed in (QA-SEC-1). iOS
 * keeps the items between launches, and a wipe resumed at launch (the app was
 * killed while signing out) runs no in-memory hook; only the signed-in layout
 * rebuilds them. Always sent: what this process last set says nothing about
 * what the OS kept from the previous one.
 */
export function clearQuickActionsAtLaunch(): Promise<void> {
  lastKey = null;
  return clearQuickActions();
}

// Early: before the server revoke of a log out, so a kill during it cannot
// leave conversation titles on the home screen.
registerWipeHook('quick_actions', clearQuickActions, { early: true });
