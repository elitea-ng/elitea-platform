import { useSyncExternalStore } from 'react';

import { fetchDiscovery, type DiscoveryDocument, type DiscoveryResult } from '@/api/discovery';
import { isEndingSession, registerWipeHook } from '@/security/endSession';
import { getSession } from '@/state/session';

/**
 * Public, non-secret facts about the connected workspace: its discovery
 * document. Kept in memory only; it is re-read from the server at launch and
 * whenever sign-in needs it. Wiped with the session (another workspace must
 * never see this one's endpoints).
 */
export interface WorkspaceInfo {
  origin: string;
  discovery: DiscoveryDocument;
}

let info: WorkspaceInfo | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

export function getWorkspaceInfo(): WorkspaceInfo | null {
  return info;
}

export function setWorkspaceInfo(next: WorkspaceInfo | null): void {
  if (info === next) return;
  info = next;
  emit();
}

export function subscribeWorkspaceInfo(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useWorkspaceInfo(): WorkspaceInfo | null {
  return useSyncExternalStore(subscribeWorkspaceInfo, getWorkspaceInfo, getWorkspaceInfo);
}

/** Listeners told about every freshly fetched document (policy, brand). */
type DiscoveryListener = (origin: string, document: DiscoveryDocument) => void;
const discoveryListeners = new Set<DiscoveryListener>();

export function onDiscovery(listener: DiscoveryListener): () => void {
  discoveryListeners.add(listener);
  return () => {
    discoveryListeners.delete(listener);
  };
}

/** Records a document for `origin` (connect confirmation, refresh). */
export function acceptDiscovery(origin: string, document: DiscoveryDocument): void {
  setWorkspaceInfo({ origin, discovery: document });
  for (const listener of [...discoveryListeners]) listener(origin, document);
}

let refreshing: { origin: string; promise: Promise<DiscoveryResult> } | null = null;

/**
 * Re-reads the connected workspace's discovery document (single-flight per
 * origin). The result is applied only if the workspace has not changed
 * meanwhile. Never throws.
 */
export function refreshWorkspaceInfo(fetchImpl = fetchDiscovery): Promise<DiscoveryResult> {
  const origin = getSession().serverOrigin;
  if (origin === null) return Promise.resolve({ ok: false, error: 'invalid' });
  if (refreshing?.origin === origin) return refreshing.promise;
  const promise = fetchImpl(origin)
    .then((result) => {
      // Not while the session is ending either: the wipe has already cleared
      // this store but serverOrigin is reset only at its end.
      if (result.ok && getSession().serverOrigin === origin && !getSession().ending && !isEndingSession()) {
        acceptDiscovery(origin, result.document);
      }
      return result;
    })
    .finally(() => {
      if (refreshing?.promise === promise) refreshing = null;
    });
  refreshing = { origin, promise };
  return promise;
}

registerWipeHook('workspace_info', () => setWorkspaceInfo(null));

/** Test helper. */
export function __resetWorkspaceInfoForTests(): void {
  info = null;
  refreshing = null;
  listeners.clear();
}
