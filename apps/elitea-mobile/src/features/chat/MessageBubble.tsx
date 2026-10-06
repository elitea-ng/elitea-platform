import type { ReactNode } from 'react';
import { Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { HighlightedText } from '@/features/chat/HighlightedText';
import type { ChatRole } from '@/features/chat/messageModel';
import { useShareOut } from '@/security/policy';

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
  const selectable = useShareOut();
  const mine = mineProp ?? role === 'user';
  const otherPerson = role === 'user' && !mine;
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
            ? { backgroundColor: palette.accent }
            : { backgroundColor: palette.backgroundElement, borderColor: palette.border },
        ]}>
        {author ? (
          <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.author, { color: mine ? palette.onAccent : palette.textSecondary }]}>{author}</Text>
        ) : null}
        {typeof children === 'string' && highlight ? (
          <HighlightedText
            text={children}
            people={highlight.people}
            agents={highlight.agents}
            style={[styles.text, { color: mine ? palette.onAccent : palette.text }]}
          />
        ) : typeof children === 'string' ? (
          <Text selectable={selectable} maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.text, { color: mine ? palette.onAccent : palette.text }]}>
            {children}
          </Text>
        ) : (
          children
        )}
        {status ? (
          <Text accessibilityLiveRegion="polite" maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.status, { color: mine ? palette.onAccent : palette.textSecondary }]}>
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
            <Text maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.moreText, { color: mine ? palette.onAccent : palette.textSecondary }]}>
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
  avatarText: { fontSize: 13, fontWeight: '700' },
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
  author: { fontSize: 12, fontWeight: '600' },
  text: { fontSize: 16, lineHeight: 22 },
  status: { fontSize: 13, fontStyle: 'italic' },
  more: { alignSelf: 'flex-end', paddingHorizontal: Spacing.one, minHeight: 24, justifyContent: 'center' },
  moreText: { fontSize: 13, fontWeight: '700', letterSpacing: 1 },
});
