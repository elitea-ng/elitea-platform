import * as AppIntents from 'expo-app-intents';

import type { Agent } from '@/api/queries/agents';
import { agentEntityId } from '@/features/intents/intentRouter';
import { log } from '@/lib/log';
import { maySurfaceWorkspace } from '@/features/surfaces/surfaceGate';
import { registerWipeHook } from '@/security/endSession';

/**
 * The agents Siri and Shortcuts may offer for "New chat with <agent>" and
 * "Ask <agent>" (AgentEntity in app-intents/AgentEntity.swift): the first
 * agents of the active workspace's selected project.
 *
 * Agent names are titles outside the app, so the catalog holds them only
 * when the workspace policy allows system surfaces (`allow_system_surfaces`,
 * client contract 1.3; off by default). Otherwise it is EMPTY and the intents
 * open the agent picker. The catalog lives in the app's UserDefaults; a wipe
 * or a workspace switch empties it, and pending invocations (which may carry
 * a dictated question) are dropped with it.
 */

export const AGENT_KIND = 'agent';
export const MAX_CATALOG_AGENTS = 10;
const MAX_NAME = 80;

export interface CatalogInput {
  workspaceId: string;
  projectId: number;
  allowTitles: boolean;
  agents: readonly Agent[];
}

export function buildAgentCatalog(input: CatalogInput): AppIntents.AppIntentEntity[] {
  if (!input.allowTitles) return [];
  const out: AppIntents.AppIntentEntity[] = [];
  const seen = new Set<number>();
  for (const agent of input.agents) {
    if (out.length >= MAX_CATALOG_AGENTS) break;
    if (seen.has(agent.id)) continue;
    const title = agent.name.replace(/\s+/g, ' ').trim().slice(0, MAX_NAME);
    if (title === '') continue;
    seen.add(agent.id);
    out.push({
      id: agentEntityId({ workspaceId: input.workspaceId, projectId: input.projectId, agentId: agent.id }),
      title,
      hideInSpotlight: true,
    });
  }
  return out;
}

let lastKey: string | null = null;
let chain: Promise<void> = Promise.resolve();

/**
 * Replaces the catalog with `workspaceId`'s agents (serialised; skipped when
 * unchanged). Agents are published only while that workspace is bound and not
 * being ended or switched away from (surfaceGate); an empty catalog always.
 * Never throws.
 */
export function publishAgentCatalog(entities: readonly AppIntents.AppIntentEntity[], workspaceId: string | null): Promise<void> {
  if (entities.length > 0 && !maySurfaceWorkspace(workspaceId)) return chain;
  const key = JSON.stringify(entities);
  if (key === lastKey) return chain;
  lastKey = key;
  chain = chain.then(async () => {
    try {
      if (!AppIntents.isAvailable()) return;
      await AppIntents.setEntityCatalogAsync(AGENT_KIND, [...entities]);
    } catch (error) {
      lastKey = null;
      log.warn('agent catalog not published', { error });
    }
  });
  return chain;
}

/** Test seam. */
export function __resetAgentCatalogForTests(): void {
  lastKey = null;
  chain = Promise.resolve();
}

export async function clearIntentState(): Promise<void> {
  await publishAgentCatalog([], null);
  try {
    if (AppIntents.isAvailable()) await AppIntents.clearPendingInvocationsAsync();
  } catch (error) {
    log.warn('pending intents not cleared', { error });
  }
}

registerWipeHook('app_intents', clearIntentState);
