/**
 * lib/normalizeCredential.ts — wire (`ConfigurationWire`, snake_case) to
 * domain (`entities/credential`'s `Credential`, camelCase) mapping (unit A7).
 *
 * `entities/credential/model/types.ts` defines the post-normalization shape
 * but ships no normaliser of its own (E1's scope was types + pure selectors
 * only — no generated/hand-written endpoint existed yet for this domain at
 * that point). This is that missing mapping step, owned here since the
 * `/configurations/*` wire format is this unit's to know.
 */
import type { Credential, CredentialPage } from '@/entities/credential';

import type { ConfigurationPageWire, ConfigurationWire } from '../api/configurations';

export function normalizeCredential(wire: ConfigurationWire): Credential {
  const credential: Credential = {
    id: String(wire.uid ?? wire.id ?? ''),
    type: wire.type,
  };
  return {
    ...credential,
    ...(wire.uid !== undefined ? { uid: wire.uid } : {}),
    ...(wire.uuid !== undefined ? { uuid: wire.uuid } : {}),
    ...(wire.data !== undefined ? { data: wire.data } : {}),
    ...(wire.elitea_title !== undefined ? { eliteaTitle: wire.elitea_title } : {}),
    ...(wire.label !== undefined ? { label: wire.label } : {}),
    ...(wire.shared !== undefined ? { shared: wire.shared } : {}),
    ...(wire.section !== undefined ? { section: wire.section } : {}),
    ...(wire.project_id !== undefined ? { projectId: String(wire.project_id) } : {}),
    ...(wire.is_pinned !== undefined ? { isPinned: wire.is_pinned } : {}),
  };
}

export function normalizeCredentialPage(wire: ConfigurationPageWire): CredentialPage {
  return {
    items: wire.items.map(normalizeCredential),
    total: wire.total,
    limit: wire.limit,
    offset: wire.offset,
    ...(wire.shared !== undefined
      ? { shared: { items: wire.shared.items.map(normalizeCredential), total: wire.shared.total } }
      : {}),
  };
}
