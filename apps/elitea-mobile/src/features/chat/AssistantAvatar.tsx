import { useQuery } from '@tanstack/react-query';

import { publicAgentIconUrl } from '@/api/queries/discover';
import { queryKeys } from '@/api/queries/keys';
import { useDesign } from '@/design/DesignProvider';
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
 * else, for the conversation's primary agent (or a model chat), Agent Zefirca's head; else a
 * monogram, so several icon-less agents stay distinguishable. Labelled with the AGENT's name,
 * never the mascot's. The mascot never replaces an agent's icon.
 */
export function AssistantAvatar({ name, imageUri, mascot, size }: { name: string; imageUri: string | null; mascot: boolean; size?: number }) {
  const design = useDesign();
  return <Avatar name={name} kind="agent" imageUri={imageUri} mascot={mascot} size={size ?? design.size.avatar.sm} decorative />;
}
