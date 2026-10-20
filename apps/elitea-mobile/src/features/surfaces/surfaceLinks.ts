import { identity } from '@/config/identity';

/**
 * Deep links from the system surfaces (widgets, the Live Activity) into the
 * app: `agentzefir://surface/<target>?ws=<workspace id>`.
 *
 * A link only opens a screen of the app; the device lock still covers it (the
 * lock gate sits above every route). Each link names the workspace it was
 * built for. One built for a workspace that is not active (a widget drawn
 * before a switch) opens the app's start instead of that workspace's screen.
 * A conversation link may also name its project (`&p=<project id>`): a
 * conversation is looked up in the selected project, so one built before a
 * project change opens the start too, never another project's thread.
 */

export type SurfaceTarget =
  | { kind: 'home' }
  | { kind: 'new' }
  | { kind: 'notifications' }
  | { kind: 'conversation'; ref: string; projectId?: number | null };

const CONVERSATION_REF = /^[0-9A-Za-z-]{1,64}$/;
const WORKSPACE_ID = /^[0-9A-Za-z_-]{1,64}$/;
const PROJECT_ID = /^[0-9]{1,15}$/;

function targetPath(target: SurfaceTarget): string {
  switch (target.kind) {
    case 'home':
      return 'home';
    case 'new':
      return 'new';
    case 'notifications':
      return 'notifications';
    case 'conversation':
      return `c/${target.ref}`;
  }
}

/** The link a surface opens. The workspace id is an opaque slot id, never the host. */
export function surfaceUrl(target: SurfaceTarget, workspaceId: string): string {
  if (target.kind === 'conversation' && !CONVERSATION_REF.test(target.ref)) return surfaceUrl({ kind: 'home' }, workspaceId);
  const scheme = identity.connectSchemes[0];
  const project =
    target.kind === 'conversation' && target.projectId != null && Number.isSafeInteger(target.projectId) && target.projectId >= 0
      ? `&p=${target.projectId}`
      : '';
  return `${scheme}://surface/${targetPath(target)}?ws=${encodeURIComponent(workspaceId)}${project}`;
}

export interface SurfaceLink {
  /** The app route to open. */
  href: string;
  workspaceId: string | null;
  /** The project a conversation link was built for, or null when it names none. */
  projectId: number | null;
}

/**
 * Reads a surface link as expo-router hands it over: the full URL
 * (`agentzefir://surface/notifications?ws=…`) or the path (`/surface/…`).
 * Null for anything that is not a surface link.
 */
export function parseSurfaceLink(raw: string): SurfaceLink | null {
  let rest = raw.trim();
  const schemeMatch = /^([a-z][a-z0-9+.-]*):\/\//i.exec(rest);
  if (schemeMatch) {
    const scheme = schemeMatch[1]!.toLowerCase();
    if (!identity.connectSchemes.some((s) => s.toLowerCase() === scheme)) return null;
    rest = rest.slice(schemeMatch[0].length);
  }
  rest = rest.replace(/^\/+/, '');
  const [pathPart, query = ''] = rest.split('?', 2) as [string, string?];
  const segments = pathPart.split('/').filter(Boolean);
  if (segments[0]?.toLowerCase() !== 'surface') return null;
  const params = new URLSearchParams(query);
  const ws = params.get('ws');
  const workspaceId = ws !== null && WORKSPACE_ID.test(ws) ? ws : null;
  const p = params.get('p');
  const projectId = p !== null && PROJECT_ID.test(p) ? Number(p) : null;
  const [first, second] = [segments[1]?.toLowerCase(), segments[2]];
  let href = '/';
  if (segments.length === 2 && first === 'new') href = '/new';
  else if (segments.length === 2 && first === 'notifications') href = '/notifications';
  else if (segments.length === 3 && first === 'c' && second !== undefined && CONVERSATION_REF.test(second)) href = `/c/${second}`;
  return { href, workspaceId, projectId };
}

/**
 * The route for a surface link given the active workspace: its screen when
 * the link belongs to the active workspace (or names none), else the start.
 * Before hydration (`active` unknown, a cold launch) the screen is opened:
 * the launch restores the workspace that was active, which is the one every
 * surface was last drawn for (a switch redraws them). A link naming a
 * project other than the selected one opens the start as well.
 */
export function routeForSurfaceLink(
  link: SurfaceLink,
  active: string | null,
  hydrated: boolean,
  activeProject: number | null = null,
): string {
  if (!hydrated || link.workspaceId === null) return link.href;
  if (link.workspaceId !== active) return '/';
  if (link.projectId !== null && link.projectId !== activeProject) return '/';
  return link.href;
}
