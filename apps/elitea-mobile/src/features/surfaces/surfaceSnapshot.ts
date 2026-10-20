import { identity } from '@/config/identity';
import { surfaceUrl } from '@/features/surfaces/surfaceLinks';

/**
 * What the home-screen and lock-screen widgets show: the snapshot the app
 * writes to the App Group after a sync (and whenever a count it knows
 * changes), for the ACTIVE workspace only.
 *
 * By default it holds COUNTS only: pending approvals, unread notifications
 * and running answers. Conversation titles and the workspace host appear only
 * when the workspace policy allows titles outside the app
 * (`allow_system_surfaces`, client contract 1.3; off by default). Message
 * content never reaches it. The App Group is readable by the widget
 * extension while the device is locked, which is why the default is
 * counts-only.
 *
 * A wipe (log out, revocation) and a workspace switch replace it with the
 * signed-out snapshot, so no count or title of a workspace outlives it.
 */

export const SNAPSHOT_VERSION = 1;
/** Conversations titled on the medium widget, at most. */
export const MAX_SURFACE_TITLES = 2;
const MAX_TITLE = 48;
const MAX_HOST = 40;

export interface SurfaceSnapshot {
  v: number;
  /** False: nothing to show (signed out, wiped, switching); the widget asks to open the app. */
  signedIn: boolean;
  approvals: number;
  unread: number;
  running: number;
  /**
   * The workspace host, only when titles are allowed; else ''. Never null:
   * the snapshot is stored in UserDefaults, which holds no null, and the
   * native widget module refuses a null prop (found on the simulator).
   */
  host: string;
  /** Conversations waiting for a decision, only when titles are allowed. */
  titles: string[];
  /** Tap targets (agentzefir://surface/…). */
  openUrl: string;
  approvalsUrl: string;
  notificationsUrl: string;
  /** When the counts were taken (epoch ms). */
  at: number;
}

export interface PendingConversation {
  /** The thread route reference (numeric id or uuid). */
  ref: string;
  name: string;
}

export interface SnapshotInput {
  workspaceId: string;
  /** The selected project the counts are for; a conversation link names it (`&p=`). */
  projectId: number;
  /** The workspace origin (`https://host[:port]`); shown only with titles allowed. */
  origin: string | null;
  allowTitles: boolean;
  approvals: readonly PendingConversation[];
  unread: number;
  running: number;
  now: number;
}

/** Signed out: the app's start (no workspace named). */
const SIGNED_OUT_URL = `${identity.connectSchemes[0]}://surface/home`;

export function signedOutSnapshot(now: number): SurfaceSnapshot {
  return {
    v: SNAPSHOT_VERSION,
    signedIn: false,
    approvals: 0,
    unread: 0,
    running: 0,
    host: '',
    titles: [],
    openUrl: SIGNED_OUT_URL,
    approvalsUrl: SIGNED_OUT_URL,
    notificationsUrl: SIGNED_OUT_URL,
    at: now,
  };
}

function count(value: number): number {
  return Number.isFinite(value) && value > 0 ? Math.min(Math.floor(value), 9999) : 0;
}

function cleanTitle(name: string): string | null {
  const one = name.replace(/\s+/g, ' ').trim();
  if (one === '') return null;
  return one.length > MAX_TITLE ? `${one.slice(0, MAX_TITLE - 1)}…` : one;
}

function hostOf(origin: string | null): string {
  if (origin === null) return '';
  try {
    const host = new URL(origin).host;
    return host.length > MAX_HOST ? `${host.slice(0, MAX_HOST - 1)}…` : host;
  } catch {
    return '';
  }
}

export function buildSnapshot(input: SnapshotInput): SurfaceSnapshot {
  const ws = input.workspaceId;
  const firstPending = input.approvals[0];
  const titles = input.allowTitles
    ? input.approvals
        .map((p) => cleanTitle(p.name))
        .filter((t): t is string => t !== null)
        .slice(0, MAX_SURFACE_TITLES)
    : [];
  return {
    v: SNAPSHOT_VERSION,
    signedIn: true,
    approvals: count(input.approvals.length),
    unread: count(input.unread),
    running: count(input.running),
    host: input.allowTitles ? hostOf(input.origin) : '',
    titles,
    openUrl: surfaceUrl({ kind: 'home' }, ws),
    // One conversation waiting: straight to it. Several: the conversation list.
    approvalsUrl:
      input.approvals.length === 1 && firstPending
        ? surfaceUrl({ kind: 'conversation', ref: firstPending.ref, projectId: input.projectId }, ws)
        : surfaceUrl({ kind: 'home' }, ws),
    notificationsUrl: surfaceUrl({ kind: 'notifications' }, ws),
    at: input.now,
  };
}

/** The same widget content (ignores `at`), so an unchanged snapshot is not rewritten. */
export function sameContent(a: SurfaceSnapshot | null, b: SurfaceSnapshot): boolean {
  if (a === null) return false;
  const { at: _a, ...restA } = a;
  const { at: _b, ...restB } = b;
  return JSON.stringify(restA) === JSON.stringify(restB);
}
