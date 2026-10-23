import { router, type Href } from 'expo-router';
import * as QuickActions from 'expo-quick-actions';
import { useEffect } from 'react';

import { useConversations } from '@/api/queries/conversations';
import { useUnseenCount } from '@/api/queries/notifications';
import {
  MAX_RECENT_CHATS,
  actionBelongsTo,
  applyQuickActions,
  buildQuickActions,
  type RecentChat,
} from '@/features/quickActions/quickActions';
import { quickActionsNative, type QuickActionsNative } from '@/features/quickActions/nativeQuickActions';
import { usePolicy } from '@/security/policy';
import { getSession, useSession } from '@/state/session';

/** Runs one action: opens its screen when it belongs to the active workspace. */
export function handleQuickAction(action: QuickActions.Action, navigate: (href: Href) => void = (href) => router.navigate(href)): boolean {
  const session = getSession();
  if (!actionBelongsTo(action, session.workspaceId, session.projectId)) return false;
  const href = action.params?.href;
  if (typeof href !== 'string' || !href.startsWith('/')) return false;
  navigate(href as Href);
  return true;
}

// The action that launched the app is handled once per process, not on every remount of the layout.
let initialHandled = false;

/**
 * The quick action that cold-started the app, taken once. Under the scene
 * life cycle (SDK 58) UIKit hands it to the scene, not to the app delegate's
 * launch options, so the package's `initial` constant is null and the item
 * arrives as a "perform" event before JS listens. The patched native module
 * keeps it until `takeInitial()` (patches/expo-quick-actions+6.0.2.patch);
 * `initial` stays the fallback (Android, an unpatched build).
 */
export function takeLaunchAction(native: QuickActionsNative | null = quickActionsNative()): QuickActions.Action | null {
  let kept: QuickActions.Action | null = null;
  try {
    kept = native?.takeInitial?.() ?? null;
  } catch {
    kept = null;
  }
  return kept ?? QuickActions.initial ?? null;
}

/** Test helper. */
export function __resetInitialQuickActionForTests(): void {
  initialHandled = false;
}

/**
 * Keeps the home-screen quick actions in step with the active workspace and
 * opens the screen of the one the user picked (also the one that launched
 * the app). Mounted in the signed-in layout, so it never navigates before a
 * workspace is connected.
 */
export function useQuickActions(): void {
  const workspaceId = useSession((s) => s.workspaceId);
  const projectId = useSession((s) => s.projectId);
  const allowTitles = usePolicy().policy.allow_system_surfaces;
  const unread = useUnseenCount(projectId).data ?? 0;
  const conversations = useConversations(allowTitles ? projectId : null);
  const firstPage = conversations.data?.pages[0]?.rows;

  const recentKey = allowTitles
    ? JSON.stringify((firstPage ?? []).slice(0, MAX_RECENT_CHATS).map((c): RecentChat => ({ ref: String(c.id), name: c.name })))
    : '[]';

  useEffect(() => {
    if (workspaceId === null) return;
    void applyQuickActions(
      workspaceId,
      buildQuickActions({ workspaceId, projectId, allowTitles, unread, recent: JSON.parse(recentKey) as RecentChat[] }),
    );
  }, [workspaceId, projectId, allowTitles, unread, recentKey]);

  useEffect(() => {
    // setTimeout: let the navigator mount before the launch action navigates.
    const run = (action: QuickActions.Action) => setTimeout(() => handleQuickAction(action), 0);
    if (!initialHandled) {
      initialHandled = true;
      const launch = takeLaunchAction();
      if (launch) run(launch);
    }
    const sub = QuickActions.addListener(run);
    return () => sub.remove();
  }, []);
}
