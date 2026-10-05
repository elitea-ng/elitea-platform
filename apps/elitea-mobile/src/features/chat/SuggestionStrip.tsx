import { ActivityIndicator, FlatList, Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { initialOf } from '@/features/chat/MessageBubble';

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
const ROW_HEIGHT = 48;

export function SuggestionStrip({
  state,
  onPick,
  label,
}: {
  state: SuggestionState;
  onPick: (item: Suggestion) => void;
  /** Accessibility label of the list ("People", "Agents"). */
  label: string;
}) {
  const palette = usePalette();
  if (state.items.length === 0 && !state.loading && !state.message) return null;
  return (
    <View
      accessibilityLabel={label}
      style={[
        styles.strip,
        { borderColor: palette.border, backgroundColor: palette.backgroundElement, maxHeight: ROW_HEIGHT * VISIBLE_SUGGESTIONS },
      ]}>
      {state.loading && state.items.length === 0 ? <ActivityIndicator style={styles.loading} color={palette.accent} /> : null}
      {state.message && state.items.length === 0 ? (
        <Text style={[styles.message, { color: palette.textSecondary }]}>{state.message}</Text>
      ) : null}
      <FlatList
        data={state.items}
        keyExtractor={(item) => item.key}
        keyboardShouldPersistTaps="handled"
        renderItem={({ item }) => (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel={item.subtitle ? `${item.title}, ${item.subtitle}` : item.title}
            onPress={() => onPick(item)}
            style={({ pressed }) => [styles.row, pressed ? { backgroundColor: palette.border } : null]}>
            <View style={[styles.avatar, { backgroundColor: palette.border }]}>
              <Text style={[styles.avatarText, { color: palette.text }]}>{item.initial ?? initialOf(item.title)}</Text>
            </View>
            <View style={styles.texts}>
              <Text numberOfLines={1} style={[styles.title, { color: palette.text }]}>
                {item.title}
              </Text>
              {item.subtitle ? (
                <Text numberOfLines={1} style={[styles.subtitle, { color: palette.textSecondary }]}>
                  {item.subtitle}
                </Text>
              ) : null}
            </View>
            {item.badge ? <Text style={[styles.badge, { color: palette.textSecondary }]}>{item.badge}</Text> : null}
          </Pressable>
        )}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  strip: { borderWidth: StyleSheet.hairlineWidth, borderRadius: 12, overflow: 'hidden' },
  loading: { padding: Spacing.two },
  message: { padding: Spacing.two, fontSize: 14 },
  row: { height: ROW_HEIGHT, flexDirection: 'row', alignItems: 'center', gap: Spacing.two, paddingHorizontal: Spacing.two },
  avatar: { width: 30, height: 30, borderRadius: 15, alignItems: 'center', justifyContent: 'center' },
  avatarText: { fontSize: 13, fontWeight: '700' },
  texts: { flex: 1 },
  title: { fontSize: 15, fontWeight: '500' },
  subtitle: { fontSize: 12 },
  badge: { fontSize: 12 },
});
