import { ActivityIndicator, FlatList, Pressable, StyleSheet, View } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { initialOf } from '@/features/chat/MessageBubble';
import { AVATAR_TONES, pressedOverlay, ripple, Text, toneIndex, uiPlatform } from '@/ui';

/** One row of the composer's suggestion strip (a person, an agent, a skill, a tool). */
export interface Suggestion {
  key: string;
  title: string;
  subtitle?: string | null;
  /** Shown at the right ("Attached", "Add"). */
  badge?: string | null;
  /** The avatar letter; defaults to the title's. */
  initial?: string;
}

export interface SuggestionState {
  items: Suggestion[];
  loading?: boolean;
  /** Shown instead of the rows ("Couldn't load agents."). */
  message?: string | null;
}

/** How many rows show before the strip scrolls. */
export const VISIBLE_SUGGESTIONS = 5;
/** A row's height at the standard text size (it grows with the text: minHeight, not height). */
const ROW_HEIGHT = 52;

export function SuggestionStrip({
  state,
  onPick,
  label,
  inline = false,
}: {
  state: SuggestionState;
  onPick: (item: Suggestion) => void;
  /** Accessibility label of the list ("People", "Agents"). */
  label: string;
  /**
   * Every row, unscrolled, for a strip inside a scrolling list (the new-chat
   * suggestions): a nested virtualized list would break the outer one.
   */
  inline?: boolean;
}) {
  const design = useDesign();
  const { colors, radius, scheme } = design;
  if (state.items.length === 0 && !state.loading && !state.message) return null;
  const row = (item: Suggestion) => {
    const [fill, ink] = AVATAR_TONES[scheme][toneIndex(item.title)];
    return (
      <Pressable
        key={item.key}
        accessibilityRole="button"
        accessibilityLabel={item.subtitle ? `${item.title}, ${item.subtitle}` : item.title}
        onPress={() => onPick(item)}
        android_ripple={ripple(design)}
        style={({ pressed }) => [styles.row, pressed && uiPlatform === 'ios' ? { backgroundColor: pressedOverlay(design) } : null]}>
        <View style={[styles.avatar, { backgroundColor: fill }]}>
          <Text variant="footnote" color={ink} emphasized maxFontSizeMultiplier={1}>
            {item.initial ?? initialOf(item.title)}
          </Text>
        </View>
        <View style={styles.texts}>
          <Text variant="subheadline" emphasized numberOfLines={1} maxFontSizeMultiplier={FONT_SCALE.chrome}>
            {item.title}
          </Text>
          {item.subtitle ? (
            <Text variant="caption" tone="secondary" numberOfLines={1} maxFontSizeMultiplier={FONT_SCALE.chrome}>
              {item.subtitle}
            </Text>
          ) : null}
        </View>
        {item.badge ? (
          <Text variant="caption" tone="accent" maxFontSizeMultiplier={FONT_SCALE.chrome}>
            {item.badge}
          </Text>
        ) : null}
      </Pressable>
    );
  };
  // A popover card above the composer (§4.5): the elevated surface, e2, radius 16.
  const frame = [styles.strip, design.elevation.e2, { borderColor: colors.separator, backgroundColor: colors.elevated, borderRadius: radius.md }];
  if (inline) {
    return (
      <View accessibilityLabel={label} style={frame}>
        {state.items.map(row)}
      </View>
    );
  }
  return (
    <View accessibilityLabel={label} style={[frame, { maxHeight: ROW_HEIGHT * VISIBLE_SUGGESTIONS }]}>
      {state.loading && state.items.length === 0 ? <ActivityIndicator style={styles.loading} color={colors.accent} /> : null}
      {state.message && state.items.length === 0 ? (
        <Text variant="subheadline" tone="secondary" style={styles.message}>
          {state.message}
        </Text>
      ) : null}
      <FlatList data={state.items} keyExtractor={(item) => item.key} keyboardShouldPersistTaps="handled" renderItem={({ item }) => row(item)} />
    </View>
  );
}

const styles = StyleSheet.create({
  strip: { borderWidth: StyleSheet.hairlineWidth, overflow: 'hidden' },
  loading: { padding: 12 },
  message: { padding: 12 },
  row: { minHeight: ROW_HEIGHT, flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 12, paddingVertical: 6 },
  avatar: { width: 32, height: 32, borderRadius: 16, alignItems: 'center', justifyContent: 'center' },
  texts: { flex: 1 },
});
