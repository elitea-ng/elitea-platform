import { StyleSheet, Text, View } from 'react-native';

import type { PublicAgentTag } from '@/api/queries/discover';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';

/** An agent's category and tags as small chips (display only). */
export function TagList({ category, tags, limit }: { category: string | null; tags: PublicAgentTag[]; limit?: number }) {
  const palette = usePalette();
  const shown = limit === undefined ? tags : tags.slice(0, limit);
  const more = tags.length - shown.length;
  if (category === null && shown.length === 0) return null;
  const labels = [...shown.map((t) => `#${t.name}`), ...(more > 0 ? [`+${more}`] : [])];
  return (
    <View
      style={styles.row}
      accessible
      accessibilityLabel={[category ? `Category ${category}` : null, tags.length > 0 ? `Tags ${tags.map((t) => t.name).join(', ')}` : null]
        .filter(Boolean)
        .join('. ')}>
      {category ? (
        <Text style={[styles.chip, styles.category, { color: palette.accent, borderColor: palette.accent }]}>{category}</Text>
      ) : null}
      {labels.map((label) => (
        <Text key={label} style={[styles.chip, { color: palette.textSecondary, backgroundColor: palette.backgroundElement }]}>
          {label}
        </Text>
      ))}
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', flexWrap: 'wrap', gap: Spacing.one },
  chip: { fontSize: 12, paddingHorizontal: Spacing.two, paddingVertical: 2, borderRadius: 10, overflow: 'hidden' },
  category: { borderWidth: StyleSheet.hairlineWidth },
});
