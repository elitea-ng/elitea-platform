import { StyleSheet, View } from 'react-native';

import type { PublicAgentTag } from '@/api/queries/discover';
import { useDesign } from '@/design/DesignProvider';
import { Text } from '@/ui';

/**
 * An agent's category and tags as small capsules (display only, one accessibility element). The
 * category sits on `accentSoft` (proven 4.5:1 in both schemes and every corrected brand), the tags
 * on the neutral `chip`; neither is a control, so neither has a press state or a 44 pt target.
 * `on` names the ground the list sits on: on a sheet (`elevated`, which `chip` equals in dark) a
 * tag sits on `surfaceSunken` with a hairline, so its capsule never dissolves into the sheet (S6).
 */
export function TagList({
  category,
  tags,
  limit,
  on = 'surface',
}: {
  category: string | null;
  tags: PublicAgentTag[];
  limit?: number;
  on?: 'surface' | 'elevated';
}) {
  const { colors, scheme } = useDesign();
  const tagFill = on === 'elevated' ? colors.surfaceSunken : colors.chip;
  const tagBorder = on === 'elevated' || scheme === 'light' ? StyleSheet.hairlineWidth : 0;
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
        <View style={[styles.chip, { backgroundColor: colors.accentSoft }]}>
          <Text variant="caption" color={colors.accentSoftOn} numberOfLines={1} style={styles.category}>
            {category}
          </Text>
        </View>
      ) : null}
      {labels.map((label) => (
        <View key={label} testID="tag-capsule" style={[styles.chip, { backgroundColor: tagFill, borderColor: colors.separator, borderWidth: tagBorder }]}>
          <Text variant="caption" tone="secondary" numberOfLines={1}>
            {label}
          </Text>
        </View>
      ))}
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', flexWrap: 'wrap', gap: 6 },
  chip: { paddingHorizontal: 8, paddingVertical: 3, borderRadius: 9999, borderCurve: 'continuous', maxWidth: '100%' },
  category: { fontWeight: '600' },
});
