/**
 * The avatar pack (ADR-0050 decision 6, "Avatar packs").
 *
 * A version's `avatar_pack` names one uploaded media file per slot: `still`
 * (required) and one per agent state. A state with no slot shows the still
 * with the default motion pack (`MOTION_SPEC`); a state WITH a slot plays its
 * own media and the client adds no motion of its own. A version with no pack
 * shows `meta.icon_meta`, the old single icon, the same way.
 *
 * This module is pure. It takes the pack as the wire sends it (any value),
 * the old icon url and the motion `avatarMotion` chose, and answers the one
 * source to render.
 */
import { AGENT_STATE, type AgentState } from '../stream/frameTypes';
import type { AvatarMotion } from './avatarState';

/** The document version of an avatar pack. */
export const AVATAR_PACK_SCHEMA = 'elitea.avatar_pack/v1';

/** The slot names in editor order: the still, then every agent state. */
export const AVATAR_SLOTS: readonly AvatarSlotName[] = ['still', ...Object.values(AGENT_STATE)];

export type AvatarSlotName = 'still' | AgentState;

/** The url every pack media file has; anything else is not shown. */
const MEDIA_URL = /^\/api\/v2\/agent_media\/[1-9][0-9]{0,17}\/[0-9a-f]{64}\.(png|jpg|webp|svg|mp4|webm)$/;

export interface AvatarPackSlot {
  readonly url: string;
  readonly media_type?: string | undefined;
  readonly poster_url?: string | undefined;
  readonly generated_by?: string | undefined;
}

export interface AvatarPack {
  readonly schema: typeof AVATAR_PACK_SCHEMA;
  readonly slots: Readonly<Partial<Record<AvatarSlotName, AvatarPackSlot>>>;
}

/** Whether a url is pack media (`/api/v2/agent_media/{project}/{sha256}.{ext}`). */
export function isAgentMediaUrl(value: unknown): value is string {
  return typeof value === 'string' && MEDIA_URL.test(value);
}

/** Whether a pack url is a video (MP4 or WebM), read from its extension as the server does. */
export function isVideoUrl(url: string): boolean {
  return url.endsWith('.mp4') || url.endsWith('.webm');
}

function slotOf(value: unknown): AvatarPackSlot | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const raw = value as Record<string, unknown>;
  if (!isAgentMediaUrl(raw.url)) return undefined;
  const poster = isAgentMediaUrl(raw.poster_url) && !isVideoUrl(raw.poster_url) ? raw.poster_url : undefined;
  // A video with no poster has nothing to show before it plays or under
  // reduced motion; the server refuses one, and the client drops one.
  if (isVideoUrl(raw.url) && poster === undefined) return undefined;
  return {
    url: raw.url,
    media_type: typeof raw.media_type === 'string' ? raw.media_type : undefined,
    poster_url: poster,
    generated_by: typeof raw.generated_by === 'string' ? raw.generated_by : undefined,
  };
}

/**
 * The pack as the client may use it, or `undefined` for null, an unknown
 * schema, or a pack with no usable still. A slot whose url is not pack media
 * is dropped; the still must not be a video.
 */
export function avatarPackOf(value: unknown): AvatarPack | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const raw = value as Record<string, unknown>;
  if (raw.schema !== AVATAR_PACK_SCHEMA || typeof raw.slots !== 'object' || raw.slots === null) return undefined;
  const rawSlots = raw.slots as Record<string, unknown>;
  const slots: Partial<Record<AvatarSlotName, AvatarPackSlot>> = {};
  for (const name of AVATAR_SLOTS) {
    const slot = slotOf(rawSlots[name]);
    if (slot !== undefined) slots[name] = slot;
  }
  if (slots.still === undefined || isVideoUrl(slots.still.url)) return undefined;
  return { schema: AVATAR_PACK_SCHEMA, slots };
}

/** The still to show: the pack's, else the old `meta.icon_meta` url. */
export function avatarStillUrl(pack: unknown, iconUrl: string | undefined): string | undefined {
  return avatarPackOf(pack)?.slots.still?.url ?? iconUrl;
}

/** What to render for one avatar. */
export interface AvatarSource {
  /** `video` is a muted, looping <video> with `poster`; `image` is an <img>. */
  readonly kind: 'image' | 'video';
  readonly src: string;
  readonly poster?: string | undefined;
  /**
   * The source is the state's own media, which carries its motion. The
   * client then adds NO default motion; when false it applies the motion
   * pack (`MOTION_SPEC`) over the still as before.
   */
  readonly ownMotion: boolean;
}

/**
 * The one source an avatar renders for the motion `avatarMotion` chose.
 * `still` (reduced motion, data-saver, or an avatar that must not animate)
 * always answers the still image, so a video never plays then. `undefined`
 * when there is neither a pack nor an old icon: the client draws its glyph.
 */
export function avatarSource(pack: unknown, iconUrl: string | undefined, motion: AvatarMotion): AvatarSource | undefined {
  const usable = avatarPackOf(pack);
  const still = usable?.slots.still?.url ?? iconUrl;
  if (motion !== 'still' && usable !== undefined) {
    const slot = usable.slots[motion];
    if (slot !== undefined) {
      return isVideoUrl(slot.url)
        ? { kind: 'video', src: slot.url, poster: slot.poster_url, ownMotion: true }
        : { kind: 'image', src: slot.url, ownMotion: true };
    }
  }
  return still === undefined ? undefined : { kind: 'image', src: still, ownMotion: false };
}
