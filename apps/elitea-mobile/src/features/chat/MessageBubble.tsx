import type { ReactNode } from 'react';
import { Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { useDesign } from '@/design/DesignProvider';
import { HighlightedText } from '@/features/chat/HighlightedText';
import type { ChatRole } from '@/features/chat/messageModel';
import { useShareOut } from '@/security/policy';
import { ramp } from '@/ui';

export interface MessageBubbleProps {
  role: ChatRole;
  /**
   * Drawn as the signed-in user's own message (right, accent). Defaults to
   * `role === 'user'`; false for another person in a shared conversation.
   */
  mine?: boolean;
  author?: string | null;
  /** The body; plain text, or a rendered node (markdown). */
  children: ReactNode;
  /** A one-line status under the body ("Using web_search…", "Still generating…"). */
  status?: string | null;
  /** Shown in the danger colour under the body. */
  error?: string | null;
  footer?: ReactNode;
  /** Names whose `@`/`#` tokens are set off in a plain-text body. */
  highlight?: { people: readonly string[]; agents: readonly string[] } | null;
  /**
   * Opens the message actions (copy, share, feedback, …): on a long press of
   * the bubble, and from the "More" button under it, which is also how
   * VoiceOver reaches them.
   */
  onActions?: (() => void) | null;
}

/** The avatar letter for a person's name. */
export function initialOf(name: string | null | undefined): string {
  const first = (name ?? '').trim().codePointAt(0);
  return first === undefined ? '?' : String.fromCodePoint(first).toUpperCase();
}

export function MessageBubble({ role, mine: mineProp, author, children, status, error, footer, highlight, onActions }: MessageBubbleProps) {
  const palette = usePalette();
  const { colors, scheme } = useDesign();
  const selectable = useShareOut();
  const mine = mineProp ?? role === 'user';
  const otherPerson = role === 'user' && !mine;
  // §4.6: the bubble tokens, not the accent, so a dark (and branded dark) user bubble is the
  // deep bubbleUser tone instead of a bright accent slab; agent bubbles have a hairline only
  // in light mode (a surface step in dark).
  const onBubble = mine ? colors.bubbleUserText : colors.bubbleAgentText;
  const quiet = mine ? colors.bubbleUserText : colors.textSecondary;
  return (
    <View style={[styles.row, mine ? styles.mine : styles.theirs]}>
      {otherPerson ? (
        <View
          accessibilityElementsHidden
          importantForAccessibility="no-hide-descendants"
          style={[styles.avatar, { backgroundColor: palette.border }]}>
          <Text style={[styles.avatarText, { color: palette.text }]}>{initialOf(author)}</Text>
        </View>
      ) : null}
      <Pressable
        accessible={false}
        disabled={!onActions}
        onLongPress={onActions ?? undefined}
        delayLongPress={350}
        style={[
          styles.bubble,
          mine
            ? { backgroundColor: colors.bubbleUser }
            : {
                backgroundColor: colors.bubbleAgent,
                borderColor: scheme === 'light' ? colors.bubbleAgentBorder : 'transparent',
              },
        ]}>
        {author ? (
          <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.author, { color: quiet }]}>{author}</Text>
        ) : null}
        {typeof children === 'string' && highlight ? (
          <HighlightedText
            text={children}
            people={highlight.people}
            agents={highlight.agents}
            style={[styles.text, { color: onBubble }]}
          />
        ) : typeof children === 'string' ? (
          <Text selectable={selectable} maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.text, { color: onBubble }]}>
            {children}
          </Text>
        ) : (
          children
        )}
        {status ? (
          <Text accessibilityLiveRegion="polite" maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.status, { color: quiet }]}>
            {status}
          </Text>
        ) : null}
        {error ? (
          <Text accessibilityRole="alert" maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.status, { color: palette.danger }]}>
            {error}
          </Text>
        ) : null}
        {footer}
        {onActions ? (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel={role === 'assistant' ? 'Answer actions' : 'Message actions'}
            onPress={onActions}
            hitSlop={10}
            style={styles.more}>
            <Text maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.moreText, { color: quiet }]}>
              •••
            </Text>
          </Pressable>
        ) : null}
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  row: { paddingHorizontal: Spacing.three, paddingVertical: Spacing.one, flexDirection: 'row', alignItems: 'flex-end', gap: Spacing.one },
  avatar: { width: 28, height: 28, borderRadius: 14, alignItems: 'center', justifyContent: 'center' },
  avatarText: { ...ramp.footnote, fontWeight: '700' },
  mine: { justifyContent: 'flex-end' },
  theirs: { justifyContent: 'flex-start' },
  bubble: {
    maxWidth: '88%',
    borderRadius: 14,
    paddingHorizontal: Spacing.three - 4,
    paddingVertical: Spacing.two,
    gap: Spacing.one,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: 'transparent',
  },
  author: { ...ramp.caption, fontWeight: '600' },
  text: { ...ramp.body },
  status: { ...ramp.footnote, fontStyle: 'italic' },
  more: { alignSelf: 'flex-end', paddingHorizontal: Spacing.one, minHeight: 24, justifyContent: 'center' },
  moreText: { ...ramp.footnote, fontWeight: '700', letterSpacing: 1 },
});
