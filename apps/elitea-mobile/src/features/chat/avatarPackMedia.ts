import { avatarPackOf, avatarSource, type AvatarMotion, type AvatarPack } from '@elitea/client-core/conversation';

import type { components } from '@/api/generated/elitea.gen';
import type { BaseFetch } from '@/api/middleware';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { rasterTypeOf } from '@/features/discover/agentIconData';
import { base64Of, fetchBrandAsset } from '@/theme/brandAssets';

/**
 * An agent's avatar pack on mobile (ADR-0050 decision 6, issue 1835).
 *
 * Client contract 1.46: the participants read serves the version's pack as
 * `persona.avatar_pack` (an object, although the 1.38 lock keeps that property typed as a
 * string), and the pack media route (`getAgentMedia`) belongs to the client contract. The app
 * reads the pack only from a server that speaks 1.46.
 *
 * What this build can draw, from `avatarSource` (`@elitea/client-core/conversation`):
 * - a still image slot (PNG, JPEG, static WebP) replaces the default motion for its state;
 * - an animated WebP or a video slot (MP4, WebM) cannot play here: React Native's `Image` does
 *   not animate WebP (Android ships `expo.webp.animated=false`, iOS animates GIF only) and the
 *   app has no video component (expo-video needs a new native build). An animated WebP shows the
 *   still and a video its poster, with the default motion over it, so the state still reads;
 * - an SVG is not drawn: an SVG slot falls back to the still, an SVG still to the old icon
 *   (`meta.icon_meta`) or the monogram;
 * - reduced motion: `avatarMotion` answers `still`, so only the still is downloaded and shown.
 */
export const AVATAR_PACK_CONTRACT = { major: 1, minor: 46 } as const;

/** An animation is at most 2 MiB (the server's `agentmedia` limit); a still is smaller. */
export const MAX_AGENT_MEDIA_BYTES = 2 * 1024 * 1024;

type Participant = components['schemas']['ConversationParticipant'];

/** The participant's usable avatar pack, or undefined (none, an unknown schema, no usable still). */
export function participantAvatarPack(participant: Participant | undefined): AvatarPack | undefined {
  // Contract 1.46: the lock types it as a string; the server sends an object. Read it as unknown.
  return avatarPackOf((participant?.persona as { avatar_pack?: unknown } | undefined)?.avatar_pack);
}

/** What one avatar downloads and shows for a state. */
export interface PackMediaPlan {
  /** `/api/v2/agent_media/{project}/{sha256}.{ext}`: a PNG, JPEG or WebP. */
  readonly path: string;
  /** The state's own media: the default motion pack is not drawn over it. */
  readonly ownMotion: boolean;
}

const drawable = (path: string) => !path.endsWith('.svg');

/**
 * The pack media to show for `motion`, or null when the pack has nothing the app can draw (no
 * pack, or an SVG still): the caller then shows the old icon or the monogram.
 */
export function packMediaPlan(pack: unknown, motion: AvatarMotion): PackMediaPlan | null {
  const usable = avatarPackOf(pack);
  if (usable === undefined) return null;
  const still = usable.slots.still?.url;
  const stillPlan = still !== undefined && drawable(still) ? { path: still, ownMotion: false } : null;
  const source = avatarSource(usable, undefined, motion);
  if (source === undefined) return stillPlan;
  if (source.kind === 'video') {
    // No video component in this build: the poster, with the default motion so the state reads.
    return source.poster !== undefined && drawable(source.poster) ? { path: source.poster, ownMotion: false } : stillPlan;
  }
  return drawable(source.src) ? { path: source.src, ownMotion: source.ownMotion } : stillPlan;
}

/** A checked pack media file: a `data:` URI to draw, or `animated` (an animated WebP this build cannot play). */
export type AgentMedia = { readonly kind: 'image'; readonly uri: string } | { readonly kind: 'animated' };

/** Whether WebP bytes are an animation: a VP8X header with the animation flag (RFC 9649 §2.7). */
export function isAnimatedWebp(bytes: Uint8Array): boolean {
  const at = (i: number, text: string) => [...text].every((ch, k) => bytes[i + k] === ch.charCodeAt(0));
  return bytes.length >= 21 && at(0, 'RIFF') && at(8, 'WEBP') && at(12, 'VP8X') && (bytes[20] & 0x02) !== 0;
}

/**
 * One pack media file, or null. Downloaded the way Discover's icons are (SEC-3): on the workspace
 * origin only, redirects refused, no credential, a 200 with a raster Content-Type, at most
 * MAX_AGENT_MEDIA_BYTES, and the bytes must start with a PNG, JPEG or WebP signature. Only those
 * bytes reach `<Image>`; an animated WebP keeps no bytes at all.
 */
export async function fetchAgentMedia(uri: string, fetchImpl: BaseFetch = noRedirectFetch): Promise<AgentMedia | null> {
  const bytes = await fetchBrandAsset(uri, /^image\/(png|jpeg|webp)$/, MAX_AGENT_MEDIA_BYTES, fetchImpl);
  if (bytes === null) return null;
  const mime = rasterTypeOf(bytes);
  if (mime === null) return null;
  if (mime === 'image/webp' && isAnimatedWebp(bytes)) return { kind: 'animated' };
  return { kind: 'image', uri: `data:${mime};base64,${base64Of(bytes)}` };
}
