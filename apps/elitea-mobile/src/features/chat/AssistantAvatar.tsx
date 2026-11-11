import { useQuery } from '@tanstack/react-query';

import type { AvatarMotion } from '@elitea/client-core/conversation';

import { publicAgentIconUrl } from '@/api/queries/discover';
import { queryKeys } from '@/api/queries/keys';
import { useDesign } from '@/design/DesignProvider';
import { AgentMotion } from '@/features/chat/AgentMotion';
import { fetchAgentMedia, packMediaPlan, type AgentMedia, type AgentMediaOptions } from '@/features/chat/avatarPackMedia';
import { fetchPublicAgentIcon } from '@/features/discover/agentIconData';
import { useSession } from '@/state/session';
import { Avatar } from '@/ui';

/**
 * The agent's own icon as a checked `data:` URI, or null (no icon, another host, a refused or
 * undecodable download). Downloaded like Discover's icons (SEC-3): redirects refused, only
 * PNG/JPEG/WebP bytes reach `<Image>`. Cached under the project's agent queries, so the session
 * wipe drops it with them.
 */
export function useAgentIconUri(projectId: number | null, iconPath: string | null, fetchIcon = fetchPublicAgentIcon): string | null {
  const origin = useSession((s) => s.serverOrigin);
  const uri = publicAgentIconUrl(iconPath, origin);
  const icon = useQuery({
    queryKey: [...queryKeys.agents(projectId ?? 0), 'icon', uri ?? ''],
    enabled: uri !== null && projectId !== null,
    staleTime: Infinity,
    retry: false,
    queryFn: () => fetchIcon(uri as string),
  });
  return uri !== null ? (icon.data ?? null) : null;
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
export type FetchAgentMedia = (uri: string, options?: AgentMediaOptions) => Promise<AgentMedia | null>;

/**
 * One avatar pack media file (`/api/v2/agent_media/...`) after its download check, or null while
 * it loads or when it is refused. Kept for good under the project's agent queries (the url is a
 * content address, so it never changes): no stale time and no garbage collection, so a state's
 * file is not downloaded again on a later turn. The session wipe and a project switch drop it.
 */
export function useAgentMedia(
  projectId: number | null,
  path: string | null,
  fetchMedia: FetchAgentMedia = fetchAgentMedia,
  options: AgentMediaOptions = {},
): AgentMedia | null {
  const origin = useSession((s) => s.serverOrigin);
  const uri = publicAgentIconUrl(path, origin);
  const still = options.still === true;
  const media = useQuery({
    queryKey: [...queryKeys.agents(projectId ?? 0), 'media', still ? 'still' : 'state', uri ?? ''],
    enabled: uri !== null && projectId !== null,
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
    queryFn: () => fetchMedia(uri as string, { still }),
  });
  return uri !== null ? (media.data ?? null) : null;
}

/**
 * An answering agent's avatar with its pack (ADR-0050 decision 6, issue 1835): a state's own still
 * image, with no default motion over it; else the pack's still with the default motion; else the
 * old icon (`imageUri`, from `meta.icon_meta`), the mark or the monogram, as before. While a
 * state's file downloads, and for one this build cannot play (an animated WebP, a video: see
 * avatarPackMedia.ts), the still shows with the default motion; a video shows its poster. Reduced
 * motion reaches here as `motion="still"`, so only the still is downloaded and shown.
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
  /** `persona.avatar_pack` as the wire sends it; undefined for none or a server before 1.46. */
  pack: unknown;
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
  const stillMedia = useAgentMedia(projectId, still?.path ?? null, fetchMedia, { still: true });
  const stateMedia = useAgentMedia(projectId, state?.path ?? null, fetchMedia);
  const stateUri = state !== null && stateMedia?.kind === 'image' ? stateMedia.uri : null;
  const stillUri = stillMedia?.kind === 'image' ? stillMedia.uri : null;
  const ownImage = (stateUri !== null && state?.ownMotion === true) || (sameAsStill && planned?.ownMotion === true && stillUri !== null);
  return (
    <AgentMotion motion={ownImage ? 'still' : motion} size={edge}>
      <Avatar name={name} kind="agent" imageUri={stateUri ?? stillUri ?? imageUri} mark={mark} size={edge} decorative />
    </AgentMotion>
  );
}
