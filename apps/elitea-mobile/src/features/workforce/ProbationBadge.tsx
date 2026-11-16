import { StyleSheet, View } from 'react-native';

import { useAgentProbation } from '@/api/queries/workforce';
import { useDesign } from '@/design/DesignProvider';
import { useWorkforceAvailable } from '@/features/workforce/useWorkforceAvailable';
import { parseServerTime, formatShortDate } from '@/lib/time';
import { Icon, Text } from '@/ui';

/** "On probation until Nov 24", from the probation end the server sent. */
export function probationText(until: string | null): string {
  const ms = parseServerTime(until);
  return ms === null ? 'On probation' : `On probation until ${formatShortDate(ms)}`;
}

/**
 * The probation badge of an agent the workforce policy hired and nobody has kept yet (ADR-0050
 * decision 5c; a hired Marketplace listing included). Read-only text: a manager keeps or undoes
 * the hire on the web or desktop. Nothing is drawn for any other agent, or on a workspace before
 * client contract 1.56.
 */
export function ProbationBadge({ projectId, agentId }: { projectId: number | null; agentId: number | null }) {
  const { colors, radius } = useDesign();
  const available = useWorkforceAvailable();
  const probation = useAgentProbation(projectId, agentId, available);
  if (!probation.data) return null;
  return (
    <View
      testID="probation-badge"
      accessible
      accessibilityLabel={probationText(probation.data.until)}
      style={[styles.badge, { backgroundColor: colors.warningSoft, borderRadius: radius.pill }]}>
      <Icon name="pauseCircle" size={12} tone="warning" />
      <Text variant="caption" tone="warning">
        {probationText(probation.data.until)}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  badge: { flexDirection: 'row', alignItems: 'center', gap: 4, paddingHorizontal: 8, paddingVertical: 2, alignSelf: 'flex-start' },
});
