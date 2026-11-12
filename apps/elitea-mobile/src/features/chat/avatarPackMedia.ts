import { avatarPackOf, isVideoUrl, type AvatarMotion, type AvatarPack } from '@elitea/client-core/conversation';

import type { components } from '@/api/generated/elitea.gen';
import type { BaseFetch } from '@/api/middleware';
import { noRedirectFetch } from '@/api/noRedirectFetch';
import { agentMediaFileName, agentMediaFiles, type AgentMediaFiles } from '@/features/chat/agentMediaFiles';
import { fetchVettedMedia, type VettedMediaType } from '@/features/media/vettedMedia';
import { base64Of } from '@/theme/brandAssets';

/**
 * An agent's avatar pack on mobile (ADR-0050 decision 6, issues 1835 and 1864).
 *
 * Client contract 1.49 serves the version's pack as `persona.avatar_pack_v1`, an
 * `AgentAvatarPackV1` object (#1881). Servers 1.46 to 1.48 sent the same object as
 * `persona.avatar_pack`, the string 1.38 reserved; the app reads that key only when the new one is
 * absent. The pack media route (`getAgentMedia`) belongs to the client contract since 1.46, and the
 * app reads a pack only from a server that speaks 1.46.
 *
 * What the app draws for a state (`packMediaPlan`):
 * - a still image slot (PNG, JPEG, static WebP) or an animated WebP slot, with expo-image, which
 *   plays an animated WebP on both platforms; the state's own media carries its motion, so the
 *   default motion pack is not drawn over it;
 * - a video slot (MP4, WebM): a muted, looping expo-video player over its poster, no default motion;
 * - an SVG is not drawn: an SVG slot falls back to the still, an SVG still to the old icon
 *   (`meta.icon_meta`) or the monogram;
 * - reduced motion: `avatarMotion` answers `still`, so only the still is downloaded, and it is shown
 *   without playing (an animated WebP still shows its first frame).
 */
export const AVATAR_PACK_CONTRACT = { major: 1, minor: 46 } as const;

/** An animation is at most 2 MiB (the server's `agentmedia` limit); a still is smaller. */
export const MAX_AGENT_MEDIA_BYTES = 2 * 1024 * 1024;

type Participant = components['schemas']['ConversationParticipant'];

/** The participant's usable avatar pack, or undefined (none, an unknown schema, no usable still). */
export function participantAvatarPack(participant: Participant | undefined): AvatarPack | undefined {
  const persona = participant?.persona as { avatar_pack_v1?: unknown; avatar_pack?: unknown } | undefined;
  // 1.49 sends `avatar_pack_v1`; a 1.46 to 1.48 server sent the object as `avatar_pack`.
  return avatarPackOf(persona?.avatar_pack_v1 ?? persona?.avatar_pack);
}

/**
 * Every agent participant's usable pack by participant id, validated once (issue 1883): the thread
 * builds it once per conversation read, not once per avatar per render.
 */
export function avatarPacksById(participants: readonly Participant[] | undefined): ReadonlyMap<number, AvatarPack> {
  const packs = new Map<number, AvatarPack>();
  for (const participant of participants ?? []) {
    const pack = participantAvatarPack(participant);
    const id = Number(participant.id);
    if (pack !== undefined && Number.isFinite(id)) packs.set(id, pack);
  }
  return packs;
}

/** What one avatar downloads and shows for a state. */
export interface PackMediaPlan {
  /** `/api/v2/agent_media/{project}/{sha256}.{ext}`: a PNG, JPEG, WebP, MP4 or WebM. */
  readonly path: string;
  /** `video` plays muted and looping over `poster`; `image` is drawn (and an animated WebP played). */
  readonly kind: 'image' | 'video';
  /** A video's poster: its own, else the still. */
  readonly poster?: string;
  /** The state's own media: the default motion pack is not drawn over it. */
  readonly ownMotion: boolean;
}

const drawable = (path: string) => !path.endsWith('.svg');

/**
 * The pack media to show for `motion`, from an already validated pack (`avatarPackOf`), or null
 * when there is nothing the app can draw (no pack, or an SVG still): the caller then shows the old
 * icon or the monogram.
 */
export function packMediaPlan(pack: AvatarPack | undefined, motion: AvatarMotion): PackMediaPlan | null {
  if (pack === undefined) return null;
  const still = pack.slots.still?.url;
  const stillPlan: PackMediaPlan | null = still !== undefined && drawable(still) ? { path: still, kind: 'image', ownMotion: false } : null;
  const slot = motion === 'still' ? undefined : pack.slots[motion];
  if (slot === undefined || !drawable(slot.url)) return stillPlan;
  if (isVideoUrl(slot.url)) {
    const poster = slot.poster_url !== undefined && drawable(slot.poster_url) ? slot.poster_url : stillPlan?.path;
    return { path: slot.url, kind: 'video', ...(poster !== undefined ? { poster } : {}), ownMotion: true };
  }
  return { path: slot.url, kind: 'image', ownMotion: true };
}

/** A checked pack media file the view can draw: a file (or, if it could not be stored, `data:`) URI. */
export interface AgentMedia {
  readonly kind: 'image' | 'video';
  readonly uri: string;
}

const TYPE_OF_EXTENSION: Record<string, VettedMediaType> = {
  png: 'image/png',
  jpg: 'image/jpeg',
  webp: 'image/webp',
  mp4: 'video/mp4',
  webm: 'video/webm',
};

/**
 * One pack media file, or null. Downloaded the SEC-3 way (`fetchVettedMedia`): on the workspace
 * origin only, redirects refused, no credential, a 200 whose Content-Type and leading bytes are
 * both the type the url's extension names, at most MAX_AGENT_MEDIA_BYTES. The checked bytes are
 * written to the app's caches (`agentMediaFiles`), and a file already there is used without a
 * download: the name is the server's content address. An image that could not be stored is drawn
 * from a `data:` URI instead; a video needs a file.
 */
export function fetchAgentMedia(
  uri: string,
  fetchImpl: BaseFetch = noRedirectFetch,
  files: AgentMediaFiles = agentMediaFiles,
): Promise<AgentMedia | null> {
  // The same content address asked for twice at once (two avatars of one agent, a state naming the
  // still's file) is one download and one write; the entry is dropped when it settles.
  const key = uri;
  const running = inFlight.get(key);
  if (running !== undefined && running.files === files) return running.promise;
  const promise = downloadAgentMedia(uri, fetchImpl, files).finally(() => {
    if (inFlight.get(key)?.promise === promise) inFlight.delete(key);
  });
  inFlight.set(key, { files, promise });
  return promise;
}

const inFlight = new Map<string, { files: AgentMediaFiles; promise: Promise<AgentMedia | null> }>();

async function downloadAgentMedia(uri: string, fetchImpl: BaseFetch, files: AgentMediaFiles): Promise<AgentMedia | null> {
  const name = agentMediaFileName(uri);
  const type = name === null ? undefined : TYPE_OF_EXTENSION[name.slice(name.lastIndexOf('.') + 1)];
  if (type === undefined) return null;
  const kind = type.startsWith('video/') ? 'video' : 'image';
  const cached = files.cached(uri);
  if (cached !== null) return { kind, uri: cached };
  const media = await fetchVettedMedia(uri, { maxBytes: MAX_AGENT_MEDIA_BYTES, types: [type] }, fetchImpl);
  if (media === null) return null;
  const stored = files.store(uri, media.bytes);
  if (stored !== null) return { kind, uri: stored };
  return kind === 'image' ? { kind, uri: `data:${media.type};base64,${base64Of(media.bytes)}` } : null;
}
