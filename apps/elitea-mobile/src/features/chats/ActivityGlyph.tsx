import { ActivityIndicator, StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { ActivityGlyphKind } from '@/features/chats/activity';
import { Icon } from '@/ui';

const SIZE = 16;

/**
 * A Chats row's activity glyph (client contract 1.12), the mobile twin of the web's
 * `ConversationActivityGlyph`: a spinner while a turn runs, an amber ring with a dot when the
 * conversation waits for the user, a filled check for an unread answer, a light check for a read
 * one. Colours are design tokens, so a brand pack and the dark scheme re-theme it. Decorative: the
 * row's spoken label carries the state (`ACTIVITY_LABEL`).
 */
export function ActivityGlyph({ kind }: { kind: ActivityGlyphKind }) {
  const { colors } = useDesign();
  return (
    <View testID={`activity-${kind}`} accessible={false} importantForAccessibility="no-hide-descendants" style={styles.box}>
      {kind === 'running' ? <ActivityIndicator size="small" color={colors.textSecondary} /> : null}
      {kind === 'needs_you' ? (
        <View style={[styles.ring, { borderColor: colors.warning }]}>
          <View style={[styles.dot, { backgroundColor: colors.warning }]} />
        </View>
      ) : null}
      {kind === 'unread' ? <Icon name="checkCircle" size={SIZE} color={colors.textPrimary} scales={false} /> : null}
      {kind === 'done' ? <Icon name="check" size={SIZE - 2} tone="tertiary" weight="semibold" scales={false} /> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  box: { minWidth: SIZE, minHeight: SIZE, alignItems: 'center', justifyContent: 'center' },
  ring: { width: SIZE - 2, height: SIZE - 2, borderRadius: SIZE, borderWidth: 2, alignItems: 'center', justifyContent: 'center' },
  dot: { width: 5, height: 5, borderRadius: 3 },
});
