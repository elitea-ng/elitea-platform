import { useSyncExternalStore } from 'react';
import { z } from 'zod';

import { log } from '@/lib/log';
import { SECURE_KEYS, getSecureItemIn } from '@/security/secureStorage';
import { getRegistrySnapshot, readRegistry, subscribeRegistry, type Registry } from '@/security/workspaceRegistry';
import { acceptImage, type BrandImage } from '@/theme/brandPack';

/**
 * What the workspace switcher shows for each connected workspace: the host
 * (always), the brand's product name and mark when the workspace has a brand
 * of its own, and whether it is signed in. Read from each slot's own
 * non-secret items (origin, persisted brand theme, credential kind); the
 * credential itself is never read here.
 */
export interface WorkspaceSummary {
  id: string;
  origin: string;
  /** The brand's product name, or null for an unbranded workspace (show the host). */
  name: string | null;
  logoMark: BrandImage | null;
  signedIn: boolean;
}

const PersistedBrand = z.looseObject({
  origin: z.string(),
  productName: z.string().max(80),
  productDefault: z.boolean(),
  logoMark: z.object({ uri: z.string(), kind: z.enum(['svg', 'raster']) }).nullable(),
});

let list: readonly WorkspaceSummary[] = Object.freeze([]);
const listeners = new Set<() => void>();

function publish(next: readonly WorkspaceSummary[]): void {
  list = Object.freeze(next);
  for (const listener of [...listeners]) listener();
}

export function getWorkspaceList(): readonly WorkspaceSummary[] {
  return list;
}

export function subscribeWorkspaceList(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useWorkspaceList(): readonly WorkspaceSummary[] {
  return useSyncExternalStore(subscribeWorkspaceList, getWorkspaceList, getWorkspaceList);
}

/** One slot's summary; null when its origin cannot be read (it is left out). */
export async function readWorkspaceSummary(id: string): Promise<WorkspaceSummary | null> {
  const origin = await getSecureItemIn(id, SECURE_KEYS.serverOrigin);
  if (origin === null) return null;
  let name: string | null = null;
  let logoMark: BrandImage | null = null;
  try {
    const raw = await getSecureItemIn(id, SECURE_KEYS.brandTheme);
    const brand = raw === null ? null : PersistedBrand.safeParse(JSON.parse(raw));
    if (brand?.success && brand.data.origin === origin) {
      name = brand.data.productDefault ? null : brand.data.productName.trim() || null;
      // Re-validated like a server value: same-origin images only.
      logoMark = brand.data.logoMark ? acceptImage(brand.data.logoMark.uri, origin) : null;
    }
  } catch {
    // An unreadable brand only costs the decoration.
  }
  const kind = await getSecureItemIn(id, SECURE_KEYS.credentialKind);
  return { id, origin, name, logoMark, signedIn: kind === 'oauth' || kind === 'pat' };
}

let refreshing: Promise<void> | null = null;
let again = false;

/** Re-reads every slot of the registry (coalesced). Never throws. */
export function refreshWorkspaceList(registry?: Registry): Promise<void> {
  if (refreshing) {
    again = true;
    return refreshing;
  }
  refreshing = (async () => {
    let source = registry;
    do {
      again = false;
      try {
        const r = source ?? (await readRegistry());
        source = undefined;
        const out: WorkspaceSummary[] = [];
        for (const id of r.ids) {
          const summary = await readWorkspaceSummary(id).catch(() => null);
          if (summary) out.push(summary);
        }
        publish(out);
      } catch (error) {
        log.info('workspace list not refreshed', { error });
      }
    } while (again);
  })().finally(() => {
    refreshing = null;
  });
  return refreshing;
}

// Any change of the registry (connect, switch, remove, wipe) refreshes the list.
subscribeRegistry(() => {
  void refreshWorkspaceList(getRegistrySnapshot());
});

/** Test helper. */
export function __resetWorkspaceListForTests(): void {
  list = Object.freeze([]);
  refreshing = null;
  again = false;
  listeners.clear();
}
