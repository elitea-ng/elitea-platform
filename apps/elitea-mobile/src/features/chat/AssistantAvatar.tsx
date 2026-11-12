import { useEventListener } from 'expo';
import { Image as ExpoImage } from 'expo-image';
import { VideoView, useVideoPlayer } from 'expo-video';
import { useState, type ReactElement } from 'react';
import { StyleSheet, View } from 'react-native';

import type { AvatarMotion, AvatarPack } from '@elitea/client-core/conversation';

import { useDesign } from '@/design/DesignProvider';
import { AgentMotion } from '@/features/chat/AgentMotion';
import { fetchAgentMedia, packMediaPlan, type AgentMedia } from '@/features/chat/avatarPackMedia';
import { fetchPublicAgentIcon } from '@/features/discover/agentIconData';
import { useVettedMedia, useVettedMediaReload } from '@/features/media/vettedMedia';
import { Avatar } from '@/ui';

/**
 * The agent's own icon as a checked `data:` URI, or null (no icon, another host, a refused or
 * undecodable download). Downloaded like Discover's icons (SEC-3, `fetchVettedMedia`): redirects
 * refused, only PNG/JPEG/WebP bytes reach `<Image>`. Cached under the project's agent queries, so
 * the session wipe drops it with them. A null `iconPath` downloads nothing.
 */
export function useAgentIconUri(projectId: number | null, iconPath: string | null, fetchIcon = fetchPublicAgentIcon): string | null {
  return useVettedMedia(projectId, 'icon', iconPath, fetchIcon);
}

/**
 * Who is answering, left of an answer (docs/design-system.md §4.6, §5): the agent's own icon;
 * else, for the conversation's primary agent (or a model chat), the Elitea mark; else a
 * monogram, so several icon-less agents stay distinguishable. Labelled with the AGENT's name,
 * never "Elitea". The mark never replaces an agent's icon.
 */
export function AssistantAvatar({ name, imageUri, mark, size }: { name: string; imageUri: string | null; mark: boolean; size?: number }) {
  const design = useDesign();
  return <Avatar name={name} kind="agent" imageUri={imageUri} mark={mark} size={size ?? design.size.avatar.sm} decorative />;
}

/** Downloads one pack media file; `fetchAgentMedia` in the app, a fake in tests. */
export type FetchAgentMedia = (uri: string) => Promise<AgentMedia | null>;

/**
 * One avatar pack media file (`/api/v2/agent_media/...`) after its download check, or null while
 * it loads or when it is refused. Kept for good under the project's agent queries (the url is a
 * content address, so it never changes), so a state's file is not looked up again on a later turn;
 * the file itself stays in the app's caches (`agentMediaFiles`). The session wipe and a project
 * switch drop the query.
 */
export function useAgentMedia(projectId: number | null, path: string | null, fetchMedia: FetchAgentMedia = fetchAgentMedia): AgentMedia | null {
  return useVettedMedia(projectId, 'media', path, fetchMedia, true);
}

/**
 * Drops the cached answer for a pack media path whose file would not load (the OS may evict the
 * caches folder under a kept query), so the next read downloads it again.
 */
export function useReloadAgentMedia(projectId: number | null): (path: string | null) => void {
  return useVettedMediaReload(projectId, 'media');
}

const hidden = { accessible: false, importantForAccessibility: 'no-hide-descendants', accessibilityElementsHidden: true } as const;

/**
 * A pack image: expo-image, which plays an animated WebP on both platforms (React Native's `Image`
 * animates GIF only on iOS and no WebP on an Expo Android build). `play` false draws the first frame.
 * The file is already on disk, so expo-image keeps it in memory only.
 */
function PackImage({ uri, size, play, onError }: { uri: string; size: number; play: boolean; onError: () => void }) {
  const frame = { width: size, height: size, borderRadius: size / 2 };
  return (
    <View {...hidden} style={[styles.frame, frame]}>
      <ExpoImage testID="avatar-image" source={{ uri }} style={frame} contentFit="cover" autoplay={play} cachePolicy="memory" onError={onError} />
    </View>
  );
}

/**
 * A pack video slot: a muted, looping player over its poster. The poster stays on top until the
 * first frame renders, and again when the player fails, so the state always reads. It never takes
 * the audio session from another app, shows no controls and does not keep the screen on.
 */
function PackVideo({ uri, poster, size, onError }: { uri: string; poster: string | null; size: number; onError: () => void }) {
  const frame = { width: size, height: size, borderRadius: size / 2 };
  const [playing, setPlaying] = useState(false);
  const player = useVideoPlayer(uri, (p) => {
    p.loop = true;
    p.muted = true;
    p.audioMixingMode = 'mixWithOthers';
    p.keepScreenOnWhilePlaying = false;
    p.play();
  });
  useEventListener(player, 'statusChange', ({ status }) => {
    if (status === 'error') {
      setPlaying(false);
      onError();
    }
  });
  return (
    <View {...hidden} pointerEvents="none" style={[styles.frame, frame]}>
      <VideoView
        testID="avatar-video"
        player={player}
        nativeControls={false}
        contentFit="cover"
        style={frame}
        onFirstFrameRender={() => setPlaying(true)}
      />
      {!playing && poster !== null ? (
        <ExpoImage testID="avatar-poster" source={{ uri: poster }} style={[StyleSheet.absoluteFill, frame]} contentFit="cover" autoplay={false} cachePolicy="memory" />
      ) : null}
    </View>
  );
}

/**
 * An answering agent's avatar with its pack (ADR-0050 decision 6, issues 1835 and 1864): a state's
 * own media (a still, an animated WebP, or a muted looping video over its poster) with no default
 * motion over it; else the pack's still with the default motion; else the old icon (`imageUri`,
 * from `meta.icon_meta`), the mark or the monogram, as before. While a state's file downloads the
 * still shows with the default motion. Reduced motion reaches here as `motion="still"`, so only the
 * still is downloaded, and it is drawn without playing.
 */
export function PackAvatar({
  projectId,
  pack,
  motion,
  name,
  imageUri,
  mark,
  size,
  fetchMedia,
}: {
  projectId: number | null;
  /** The participant's validated pack (`avatarPacksById`); undefined for none or a server before 1.46. */
  pack: AvatarPack | undefined;
  motion: AvatarMotion;
  name: string;
  imageUri: string | null;
  mark: boolean;
  size?: number;
  fetchMedia?: FetchAgentMedia;
}) {
  const design = useDesign();
  const edge = size ?? design.size.avatar.sm;
  const still = packMediaPlan(pack, 'still');
  const planned = motion === 'still' ? null : packMediaPlan(pack, motion);
  // A state that plans the still's file (no slot of its own, or a slot set to the same file)
  // downloads nothing more; a slot of its own still turns the default motion off.
  const sameAsStill = planned !== null && planned.path === still?.path;
  const state = planned !== null && !sameAsStill ? planned : null;
  const posterPath = state?.kind === 'video' && state.poster !== still?.path ? (state.poster ?? null) : null;
  const stillMedia = useAgentMedia(projectId, still?.path ?? null, fetchMedia);
  const stateMedia = useAgentMedia(projectId, state?.path ?? null, fetchMedia);
  const posterMedia = useAgentMedia(projectId, posterPath, fetchMedia);
  const reload = useReloadAgentMedia(projectId);
  const stillUri = stillMedia?.kind === 'image' ? stillMedia.uri : null;

  let drawn: ReactElement | null = null;
  let ownMotion = false;
  if (state !== null && stateMedia !== null && stateMedia.kind === state.kind) {
    ownMotion = true;
    drawn =
      stateMedia.kind === 'video' ? (
        // Keyed by the file: another state's video is a new player, with its poster up until its first frame.
        <PackVideo key={stateMedia.uri} uri={stateMedia.uri} poster={posterMedia?.kind === 'image' ? posterMedia.uri : stillUri} size={edge} onError={() => reload(state.path)} />
      ) : (
        <PackImage uri={stateMedia.uri} size={edge} play onError={() => reload(state.path)} />
      );
  } else if (stillUri !== null) {
    ownMotion = sameAsStill && planned?.ownMotion === true;
    drawn = <PackImage uri={stillUri} size={edge} play={ownMotion} onError={() => reload(still?.path ?? null)} />;
  }
  return (
    <AgentMotion motion={ownMotion ? 'still' : motion} size={edge}>
      {drawn ?? <Avatar name={name} kind="agent" imageUri={imageUri} mark={mark} size={edge} decorative />}
    </AgentMotion>
  );
}

const styles = StyleSheet.create({
  frame: { overflow: 'hidden', alignItems: 'center', justifyContent: 'center', borderCurve: 'continuous' },
});
