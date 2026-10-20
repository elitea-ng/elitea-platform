import * as AppIntents from 'expo-app-intents';
import { router, type Href } from 'expo-router';
import { useEffect } from 'react';

import { useAgents } from '@/api/queries/agents';
import { buildAgentCatalog, publishAgentCatalog } from '@/features/intents/agentCatalog';
import { handOff } from '@/features/intents/intentHandoff';
import { isStaleInvocation, routeIntent, type IntentAction } from '@/features/intents/intentRouter';
import { log } from '@/lib/log';
import { usePolicy } from '@/security/policy';
import { getSession, useSession } from '@/state/session';

export type Navigate = (href: Href) => void;

/** Opens the screen an intent asks for; /new gets the agent and question in memory (intentHandoff.ts). */
export function performIntent(action: IntentAction, navigate: Navigate = (href) => router.navigate(href)): void {
  switch (action.kind) {
    case 'open':
      navigate(action.href);
      return;
    case 'newChat':
      navigate({ pathname: '/new', params: { intent: handOff({ agent: action.agent, prefill: action.prefill }) } });
      return;
    case 'discard':
      return;
  }
}

/**
 * Handles one pending snapshot. Each invocation is REMOVED BEFORE its screen
 * opens: an intent only opens a screen (and "New chat with <agent>" starts a
 * conversation), so losing one to a crash is harmless while replaying one
 * would start a second conversation. One queued long ago (while signed out)
 * is removed and opens nothing.
 */
export async function handleInvocations(
  pending: readonly AppIntents.AppIntentInvocation[],
  navigate: Navigate = (href) => router.navigate(href),
  remove: (id: string) => Promise<void> = AppIntents.removePendingInvocationAsync,
  now: () => number = Date.now,
): Promise<void> {
  for (const invocation of pending) {
    try {
      await remove(invocation.id);
    } catch (error) {
      log.warn('intent not dequeued; skipped', { error });
      continue;
    }
    if (isStaleInvocation(invocation, now())) {
      log.info('stale intent dropped');
      continue;
    }
    performIntent(routeIntent(invocation, getSession().workspaceId), navigate);
  }
}

/**
 * Siri and Shortcuts for the active workspace: publishes the agent catalog
 * (names only when the policy allows titles) and opens the screen of each
 * invocation. Mounted in the signed-in layout, which mounts after the first
 * unlock; a relock covers whatever an intent opened, and nothing that writes
 * to the workspace runs while it is covered (src/app/(app)/new.tsx).
 */
export function useAppIntentHandler(): void {
  const workspaceId = useSession((s) => s.workspaceId);
  const projectId = useSession((s) => s.projectId);
  const allowTitles = usePolicy().policy.allow_system_surfaces;
  const agents = useAgents(allowTitles ? projectId : null);
  const loaded = agents.data;

  useEffect(() => {
    if (workspaceId === null || projectId === null) return;
    // Never overwrite a catalog with a list that has not loaded yet.
    if (allowTitles && loaded === undefined) return;
    void publishAgentCatalog(buildAgentCatalog({ workspaceId, projectId, allowTitles, agents: loaded ?? [] }), workspaceId);
  }, [workspaceId, projectId, allowTitles, loaded]);

  AppIntents.useAppIntents((pending) =>
    // setTimeout: let the navigator mount before a cold-start intent navigates.
    new Promise<void>((resolve) => setTimeout(() => void handleInvocations(pending).finally(resolve), 0)),
  );
}
