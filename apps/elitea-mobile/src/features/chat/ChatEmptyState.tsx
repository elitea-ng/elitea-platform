import { Pressable, ScrollView, StyleSheet, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { useDesign } from '@/design/DesignProvider';
import { Avatar, GradientBackground, Icon, Text } from '@/ui';

/** Suggestions for a model chat (no agent, so no conversation starters of its own). */
export const MODEL_CHAT_SUGGESTIONS = ['Summarise this for me', 'Draft a short email', 'Explain it simply', 'Brainstorm a few ideas'] as const;

/**
 * An empty conversation (docs/design-system.md §4.9, screens.md §4): on the pastel ground, the
 * agent's own icon (48) or, when it has none, Agent Zefirca (the bust, decorative), "Ask
 * {agent} anything", the agent's welcome message, and its conversation starters as glass
 * suggestion cards. A suggestion goes into the composer to edit or send; nothing is sent on
 * its own. The mascot is on screen once: the answers below it get monograms (§5).
 */
export function ChatEmptyState({
  agentName,
  iconUri,
  welcome,
  suggestions,
  onSuggestion,
  bottomInset,
}: {
  agentName: string;
  /** The agent's own icon (checked `data:` URI); null shows the mascot. */
  iconUri: string | null;
  welcome: string | null;
  suggestions: readonly string[];
  onSuggestion: (text: string) => void;
  /** Room the floating composer takes at the bottom. */
  bottomInset: number;
}) {
  const design = useDesign();
  const { space, radius, colors } = design;
  // The clamps keep the grid tidy at standard sizes; at accessibility sizes the text runs in
  // full (the content scrolls), so a long welcome or starter is never cut off (HIG).
  const clamp = (lines: number) => (design.accessibilityLayout ? undefined : lines);
  return (
    <View style={styles.fill}>
      <GradientBackground style={StyleSheet.absoluteFill} />
      <ScrollView
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={[styles.content, { padding: space.screenGutter, paddingBottom: bottomInset + space[4], gap: space[3] }]}>
        <View style={styles.hero}>
          {iconUri ? (
            <Avatar name={agentName} kind="agent" imageUri={iconUri} size={design.size.avatar.lg} decorative />
          ) : (
            <Mascot variant="bust" size={design.accessibilityLayout ? 72 : 112} decorative />
          )}
          <Text variant="title2" heading style={styles.center}>
            {`Ask ${agentName} anything`}
          </Text>
          <Text variant="callout" tone="secondary" style={styles.center} numberOfLines={clamp(6)}>
            {welcome ?? 'Type a message, attach a file or dictate. Pick a suggestion to start.'}
          </Text>
        </View>
        {suggestions.length > 0 ? (
          <View style={[styles.grid, design.accessibilityLayout ? styles.column : null, { gap: space.cardGap }]} accessibilityLabel="Suggestions">
            {suggestions.map((text) => (
              <Pressable
                key={text}
                accessibilityRole="button"
                accessibilityLabel={text}
                accessibilityHint="Puts it in the message box to edit or send"
                onPress={() => onSuggestion(text)}
                style={[
                  styles.card,
                  design.accessibilityLayout ? null : styles.half,
                  design.elevation.e1,
                  { backgroundColor: colors.surfaceGlass.opaqueFallback, borderColor: colors.separator, borderRadius: radius.card, padding: space.cardPadding },
                ]}>
                <Icon name="agent" size={18} tone="accent" />
                <Text variant="callout" numberOfLines={clamp(3)} style={styles.flex}>
                  {text}
                </Text>
                <Icon name="arrowGo" size={16} tone="secondary" />
              </Pressable>
            ))}
          </View>
        ) : null}
      </ScrollView>
    </View>
  );
}

const styles = StyleSheet.create({
  fill: { flex: 1 },
  content: { flexGrow: 1, justifyContent: 'flex-end' },
  hero: { alignItems: 'center', gap: 10, paddingBottom: 8 },
  center: { textAlign: 'center' },
  grid: { flexDirection: 'row', flexWrap: 'wrap' },
  column: { flexDirection: 'column' },
  card: { flexDirection: 'row', alignItems: 'center', gap: 10, minHeight: 64, borderWidth: StyleSheet.hairlineWidth, borderCurve: 'continuous' },
  half: { flexBasis: '47%', flexGrow: 1 },
  flex: { flex: 1 },
});
