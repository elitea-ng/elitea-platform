import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Image, StyleSheet, View } from 'react-native';

import { publicAgentIconUrl } from '@/api/queries/discover';
import { queryKeys } from '@/api/queries/keys';
import { useDesign } from '@/design/DesignProvider';
import { monogramStyle, Text } from '@/ui';
import { fetchPublicAgentIcon } from '@/features/discover/agentIconData';
import { useSession } from '@/state/session';

/** The first letter of the first two words: "Zefir Travel Planner" → "ZT". */
export function monogramOf(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean);
  const letters = words.slice(0, 2).map((w) => Array.from(w)[0] ?? '');
  return letters.join('').toUpperCase() || '?';
}

/**
 * A public agent's icon from the workspace's own origin, or its monogram when
 * it has none, it is on another host, its download is refused (a redirect, a
 * wrong type, too large) or it fails to decode. Decorative: the row or header
 * next to it carries the name.
 *
 * The icon is never handed to `<Image>` as a URL (SEC-3, agentIconData.ts):
 * it is downloaded with redirects refused and shown from its checked bytes.
 * The `data:` URI is cached under the project's Discover queries, so the
 * session wipe (query cache) drops it with the rest of the catalogue.
 */
export function AgentIcon({
  name,
  iconPath,
  projectId,
  size = 40,
  fetchIcon = fetchPublicAgentIcon,
}: {
  name: string;
  iconPath: string | null;
  /** The active project the catalogue is read under. */
  projectId: number | null;
  size?: number;
  /** Injectable for tests. */
  fetchIcon?: (uri: string) => Promise<string | null>;
}) {
  const { colors } = useDesign();
  const origin = useSession((s) => s.serverOrigin);
  const uri = publicAgentIconUrl(iconPath, origin);
  const icon = useQuery({
    queryKey: [...queryKeys.discover(projectId ?? 0), 'icon', uri ?? ''],
    enabled: uri !== null && projectId !== null,
    staleTime: Infinity,
    retry: false,
    queryFn: () => fetchIcon(uri as string),
  });
  const data = uri !== null ? (icon.data ?? null) : null;
  const [failed, setFailed] = useState<string | null>(null);
  // A squircle tile (continuous corners on iOS): an agent, never a person's round avatar.
  const frame = { width: size, height: size, borderRadius: Math.round(size * 0.28), borderCurve: 'continuous' as const };
  if (data !== null && failed !== data) {
    return (
      <Image
        testID="agent-icon-image"
        source={{ uri: data }}
        onError={() => setFailed(data)}
        accessible={false}
        style={[frame, { backgroundColor: colors.surfaceSunken }]}
      />
    );
  }
  return (
    <View testID="agent-icon-monogram" accessible={false} style={[frame, styles.center, { backgroundColor: colors.secondarySoft }]}>
      <Text maxFontSizeMultiplier={1} color={colors.secondarySoftOn} style={[monogramStyle(size, 0.38), styles.monogram]}>
        {monogramOf(name)}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({ center: { alignItems: 'center', justifyContent: 'center' }, monogram: { fontWeight: '600' } });
