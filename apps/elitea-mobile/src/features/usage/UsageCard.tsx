import { StyleSheet, View } from 'react-native';

import { totalsLine, usageLine, useUsage, type UsageSummary } from '@/api/queries/usage';
import { useDesign } from '@/design/DesignProvider';
import { Icon, ListSection, Text } from '@/ui';

/**
 * Settings > Usage: this month's spend against the budget, for the caller
 * ("You", scope=user) and the whole project, from getProjectUsage (client
 * contract 1.3). A member who may not see costs sees percentages only; the
 * token and request totals appear only when the workspace recorded them.
 * Hidden entirely when the workspace answers neither scope.
 */
export function UsageCard({ projectId }: { projectId: number }) {
  const mine = useUsage(projectId, 'user', true);
  const project = useUsage(projectId, 'project', true);
  return <UsageView you={mine.data ?? null} project={project.data ?? null} />;
}

/**
 * The usage section (as Settings shows it, and its `__DEV__` preview): one inset-grouped section
 * with a row per scope (the line, a meter when there is a percentage, the totals), a warning row
 * with a symbol when a budget is close (never colour alone), and the period as the footer.
 */
export function UsageView({ you, project }: { you: UsageSummary | null; project: UsageSummary | null }) {
  const rows = [
    { label: 'You', usage: you },
    { label: 'Project', usage: project },
  ].filter((r): r is { label: string; usage: UsageSummary } => r.usage !== null);
  if (rows.length === 0) return null;
  const period = rows[0].usage;
  return (
    <View testID="usage-card">
      <ListSection title="Usage this month" footer={`${period.periodStart} to ${period.periodEnd} (UTC); resets ${period.resetsAt.slice(0, 10)}.`}>
        {rows.map(({ label, usage }) => (
          <UsageRow key={label} label={label} usage={usage} />
        ))}
        {rows.some((r) => r.usage.warning) ? (
          <View accessible accessibilityRole="alert" style={styles.warning}>
            <Icon name="warning" size={18} tone="warning" />
            <Text variant="footnote" tone="warning" style={styles.flex}>
              Close to the limit. Requests stop when a budget is used up.
            </Text>
          </View>
        ) : null}
      </ListSection>
    </View>
  );
}

function UsageRow({ label, usage }: { label: string; usage: UsageSummary }) {
  const { colors } = useDesign();
  const line = usageLine(usage);
  const totals = totalsLine(usage);
  const pct = usage.percentUsed === null ? null : Math.min(100, Math.max(0, usage.percentUsed));
  return (
    <View
      style={styles.row}
      accessible
      accessibilityLabel={`${label}: ${line}${usage.warning ? ', close to the limit' : ''}`}
      accessibilityValue={pct !== null ? { min: 0, max: 100, now: Math.round(pct) } : undefined}>
      <View style={styles.line}>
        <Text variant="body">{label}</Text>
        <Text variant="body" tone={usage.warning ? 'warning' : 'secondary'} style={styles.value}>
          {line}
        </Text>
      </View>
      {pct !== null ? (
        <View style={[styles.track, { backgroundColor: colors.surfaceSunken, borderColor: colors.separator }]}>
          <View style={[styles.fill, { width: `${pct}%`, backgroundColor: usage.warning ? colors.warning : colors.accent }]} />
        </View>
      ) : null}
      {totals ? (
        <Text variant="footnote" tone="secondary">
          {totals}
        </Text>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  row: { paddingHorizontal: 16, paddingVertical: 12, gap: 8 },
  line: { flexDirection: 'row', flexWrap: 'wrap', gap: 8, justifyContent: 'space-between' },
  value: { flexShrink: 1, textAlign: 'right' },
  track: { height: 8, borderRadius: 4, overflow: 'hidden', borderWidth: StyleSheet.hairlineWidth },
  fill: { height: '100%', borderRadius: 4 },
  warning: { flexDirection: 'row', alignItems: 'center', gap: 10, paddingHorizontal: 16, paddingVertical: 12 },
});
