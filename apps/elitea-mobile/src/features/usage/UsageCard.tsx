import { StyleSheet, Text, View } from 'react-native';

import { totalsLine, usageLine, useUsage, type UsageSummary } from '@/api/queries/usage';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { ramp } from '@/ui';

/**
 * Settings > Usage: this month's spend against the budget, for the caller
 * ("You", scope=user) and the whole project, from getProjectUsage (client
 * contract 1.3). A member who may not see costs sees percentages only; the
 * token and request totals appear only when the workspace recorded them.
 * Hidden entirely when the workspace answers neither scope.
 */
export function UsageCard({ projectId }: { projectId: number }) {
  const palette = usePalette();
  const mine = useUsage(projectId, 'user', true);
  const project = useUsage(projectId, 'project', true);
  const rows = [
    { label: 'You', usage: mine.data ?? null },
    { label: 'Project', usage: project.data ?? null },
  ].filter((r): r is { label: string; usage: UsageSummary } => r.usage !== null);
  if (rows.length === 0) return null;
  const period = rows[0].usage;

  return (
    <View testID="usage-card" style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
      <Text style={[styles.label, { color: palette.textSecondary }]}>Usage this month</Text>
      {rows.map(({ label, usage }) => (
        <View key={label} style={styles.row} accessible accessibilityLabel={`${label}: ${usageLine(usage)}${usage.warning ? ', close to the limit' : ''}`}>
          <View style={styles.line}>
            <Text style={[styles.value, { color: palette.text }]}>{label}</Text>
            <Text style={[styles.value, styles.right, { color: usage.warning ? palette.danger : palette.text }]}>{usageLine(usage)}</Text>
          </View>
          {usage.percentUsed !== null ? (
            <View style={[styles.track, { backgroundColor: palette.border }]}>
              <View
                style={[
                  styles.fill,
                  { width: `${Math.min(100, Math.max(0, usage.percentUsed))}%`, backgroundColor: usage.warning ? palette.danger : palette.accent },
                ]}
              />
            </View>
          ) : null}
          {totalsLine(usage) ? <Text style={[styles.small, { color: palette.textSecondary }]}>{totalsLine(usage)}</Text> : null}
        </View>
      ))}
      {rows.some((r) => r.usage.warning) ? (
        <Text style={[styles.small, { color: palette.danger }]}>Close to the limit. Requests stop when a budget is used up.</Text>
      ) : null}
      <Text style={[styles.small, { color: palette.textSecondary }]}>
        {period.periodStart} to {period.periodEnd} (UTC); resets {period.resetsAt.slice(0, 10)}.
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  card: { borderRadius: 12, padding: Spacing.three, gap: Spacing.two },
  label: { ...ramp.footnote, fontWeight: '600' },
  row: { gap: Spacing.one },
  line: { flexDirection: 'row', gap: Spacing.two },
  right: { flex: 1, textAlign: 'right' },
  value: { ...ramp.body },
  small: { ...ramp.footnote },
  track: { height: 6, borderRadius: 3, overflow: 'hidden' },
  fill: { height: 6, borderRadius: 3 },
});
