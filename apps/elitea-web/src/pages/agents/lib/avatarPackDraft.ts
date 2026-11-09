/**
 * ADR-0050 decision 6 — the Avatar tab's draft of the version's avatar pack.
 *
 * The draft is the wire object itself (`AvatarPack` from
 * `@elitea/client-core/conversation`), so nothing is converted on save. Only
 * a usable pack is held: `avatarPackOf` drops a slot whose url is not agent
 * media, and answers `null` for a pack without a still.
 */
import {
  AVATAR_PACK_SCHEMA,
  avatarPackOf,
  type AvatarPack,
  type AvatarPackSlot,
  type AvatarSlotName,
} from '@elitea/client-core/conversation';

import type { AgentAvatarPackV1 } from '@/shared/api/generated/model';

/** The stored pack as the draft holds it; `null` for none. */
export function avatarPackDraftOf(stored: unknown): AvatarPack | null {
  return avatarPackOf(stored) ?? null;
}

/**
 * The pack with `slot` set (or removed, for `undefined`). Removing the still
 * removes the whole pack: the server refuses a pack without one.
 */
export function withAvatarSlot(pack: AvatarPack | null, name: AvatarSlotName, slot: AvatarPackSlot | undefined): AvatarPack | null {
  if (name === 'still' && slot === undefined) return null;
  if (pack === null && name !== 'still') return null;
  const slots: Partial<Record<AvatarSlotName, AvatarPackSlot>> = { ...pack?.slots };
  if (slot === undefined) delete slots[name];
  else slots[name] = slot;
  return { schema: AVATAR_PACK_SCHEMA, slots };
}

function slotKey(slot: AvatarPackSlot | undefined): string {
  return slot === undefined ? '' : `${slot.url}|${slot.poster_url ?? ''}|${slot.generated_by ?? ''}`;
}

/** Slot by slot; `media_type` is the server's and is not compared. */
export function areAvatarPacksEqual(a: AvatarPack | null, b: AvatarPack | null): boolean {
  if (a === null || b === null) return a === b;
  const names = new Set<string>([...Object.keys(a.slots), ...Object.keys(b.slots)]);
  for (const name of names) {
    if (slotKey(a.slots[name as AvatarSlotName]) !== slotKey(b.slots[name as AvatarSlotName])) return false;
  }
  return true;
}

function toWire(pack: AvatarPack): AgentAvatarPackV1 {
  const slots: Record<string, { url: string; poster_url?: string; generated_by?: string }> = {};
  for (const [name, slot] of Object.entries(pack.slots)) {
    if (slot === undefined) continue;
    slots[name] = {
      url: slot.url,
      ...(slot.poster_url !== undefined ? { poster_url: slot.poster_url } : {}),
      ...(slot.generated_by !== undefined ? { generated_by: slot.generated_by } : {}),
    };
  }
  return { schema: AVATAR_PACK_SCHEMA, slots: slots as AgentAvatarPackV1['slots'] };
}

/**
 * The `avatar_pack` key of a version write. An edited draft is sent (null
 * clears the stored pack); an untouched stored pack is forwarded, so a Save
 * As Version keeps it; with neither, the key is left out and the server
 * leaves the column alone.
 */
export function avatarPackWriteField(
  stored: unknown,
  draft: AvatarPack | null | undefined,
): { avatar_pack?: AgentAvatarPackV1 | null } {
  const storedPack = avatarPackDraftOf(stored);
  if (draft !== undefined && !areAvatarPacksEqual(draft, storedPack)) {
    return { avatar_pack: draft === null ? null : toWire(draft) };
  }
  return storedPack === null ? {} : { avatar_pack: toWire(storedPack) };
}
