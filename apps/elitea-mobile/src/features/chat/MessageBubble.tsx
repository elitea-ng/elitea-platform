import type { ReactNode } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { HighlightedText } from '@/features/chat/HighlightedText';
import type { ChatRole } from '@/features/chat/messageModel';
import { useShareOut } from '@/security/policy';
import { Avatar, Icon, ramp, Text } from '@/ui';

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
  /** A one-line status under the body ("Still generating…", "Waiting to send"). */
  status?: string | null;
  /** Shown in the danger colour under the body. */
  error?: string | null;
  /** Inside the bubble, under the body: files, tool steps, a retry row. */
  footer?: ReactNode;
  /** Names whose `@`/`#` tokens are set off in a plain-text body. */
  highlight?: { people: readonly string[]; agents: readonly string[] } | null;
  /**
   * Opens the message actions (copy, share, feedback, …): on a long press of the bubble, and
   * as the bubble's accessibility action (VoiceOver "Actions" rotor, TalkBack actions menu).
   * An answer also shows them as its action row (`actions`).
   */
  onActions?: (() => void) | null;
  /**
   * The answering agent's avatar, left of an answer (its own icon, else the mascot or a
   * monogram: `AssistantAvatar`). Another person's initials are drawn here by default.
   */
  avatar?: ReactNode;
  /** Above an answer: "Research · 9:41" (the agent and the time). */
  meta?: string | null;
  /** Under the bubble, outside it: the answer's action row. */
  actions?: ReactNode;
}

/** The avatar letter for a person's name. */
export function initialOf(name: string | null | undefined): string {
  const first = (name ?? '').trim().codePointAt(0);
  return first === undefined ? '?' : String.fromCodePoint(first).toUpperCase();
}

/**
 * One message (docs/design-system.md §4.6). Mine: right-aligned, `bubbleUser`, at most 80 % wide,
 * radius 22 with a 6 corner on my side. An answer: the agent's avatar on the left, level with
 * the name line above the bubble (the same place while streaming and when finished), a full-width `bubbleAgent` (a hairline in light mode, a surface step in dark)
 * with the 6 corner on the agent's side, and the action row under it. Another person: their
 * initials avatar and name, an agent-style bubble.
 */
export function MessageBubble({
  role,
  mine: mineProp,
  author,
  children,
  status,
  error,
  footer,
  highlight,
  onActions,
  avatar,
  meta,
  actions,
}: MessageBubbleProps) {
  const design = useDesign();
  const { colors, scheme, radius, space } = design;
  const selectable = useShareOut();
  const mine = mineProp ?? role === 'user';
  const otherPerson = role === 'user' && !mine;
  // §4.6: the bubble tokens, not the accent, so a dark (and branded dark) user bubble is the
  // deep bubbleUser tone instead of a bright accent slab.
  const onBubble = mine ? colors.bubbleUserText : colors.bubbleAgentText;
  const quiet = mine ? colors.bubbleUserText : colors.textSecondary;
  const leadingAvatar = otherPerson ? (
    <Avatar name={author ?? '?'} size={design.size.avatar.sm} decorative />
  ) : role === 'assistant' ? (
    avatar
  ) : null;
  const metaLine = meta ?? (otherPerson || (role === 'assistant' && !avatar) ? author : null);
  const plain = typeof children === 'string';
  const body =
    plain && highlight ? (
      <HighlightedText text={children} people={highlight.people} agents={highlight.agents} style={[styles.text, { color: onBubble }]} />
    ) : plain ? (
      children === '' ? null : (
        <Text selectable={selectable} maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.text, { color: onBubble }]}>
          {children}
        </Text>
      )
    ) : (
      children
    );
  // A plain-text message (mine or another person's) is one element for screen readers, so its
  // actions can hang on it; an answer's markdown keeps its links reachable one by one. One with
  // a footer (its files) is not: an accessible element hides its children on iOS, and the
  // file buttons must stay reachable. The status and the error are read as part of the label.
  const speakable = plain && role === 'user' && !footer;
  const spoken = speakable
    ? [`${mine ? 'You' : (author ?? 'Someone')}: ${children as string}`, status, error].filter((part) => part).join('. ')
    : undefined;
  return (
    <View style={[styles.row, { paddingHorizontal: space.screenGutterCompact }, mine ? styles.mine : styles.theirs]}>
      {leadingAvatar ? (
        <View testID="message-avatar" style={[styles.avatarColumn, { width: design.size.avatar.sm }]}>
          {leadingAvatar}
        </View>
      ) : null}
      <View style={[styles.column, mine ? styles.columnMine : styles.columnTheirs]}>
        {metaLine ? (
          <Text variant="footnote" tone="secondary" numberOfLines={1} maxFontSizeMultiplier={FONT_SCALE.control} style={styles.meta}>
            {metaLine}
          </Text>
        ) : null}
        <Pressable
          accessible={speakable}
          accessibilityLabel={spoken}
          accessibilityActions={speakable && onActions ? [{ name: 'longpress', label: 'Message actions' }] : undefined}
          onAccessibilityAction={(event) => {
            if (event.nativeEvent.actionName === 'longpress') onActions?.();
          }}
          disabled={!onActions}
          onLongPress={onActions ?? undefined}
          delayLongPress={350}
          style={[
            styles.bubble,
            {
              borderRadius: radius.bubble,
              paddingHorizontal: space[3] + 2,
              paddingVertical: space[2] + 2,
              gap: space[1] + 2,
            },
            mine
              ? { backgroundColor: colors.bubbleUser, borderBottomEndRadius: radius.bubbleTail, maxWidth: '84%' }
              : {
                  backgroundColor: colors.bubbleAgent,
                  borderColor: scheme === 'light' ? colors.bubbleAgentBorder : 'transparent',
                  borderBottomStartRadius: radius.bubbleTail,
                  alignSelf: role === 'assistant' ? 'stretch' : 'flex-start',
                },
          ]}>
          {body}
          {status ? (
            <Text variant="footnote" accessibilityLiveRegion="polite" maxFontSizeMultiplier={FONT_SCALE.body} color={quiet}>
              {status}
            </Text>
          ) : null}
          {error ? (
            <View style={[styles.error, { backgroundColor: colors.dangerSoft, borderRadius: radius.sm }]}>
              <Icon name="error" size={16} tone="danger" />
              <Text variant="footnote" tone="danger" accessibilityRole="alert" maxFontSizeMultiplier={FONT_SCALE.body} style={styles.flex}>
                {error}
              </Text>
            </View>
          ) : null}
          {footer}
        </Pressable>
        {actions}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  // The avatar is pinned to the top, beside the name line, in every state: aligned to the
  // bottom it rode the bubble's edge while streaming, then dropped into the action row (reading
  // as one more button) when the answer finished.
  row: { paddingVertical: 6, flexDirection: 'row', alignItems: 'flex-start', gap: 8 },
  mine: { justifyContent: 'flex-end' },
  theirs: { justifyContent: 'flex-start' },
  avatarColumn: { alignItems: 'center' },
  column: { flex: 1, gap: 4 },
  columnMine: { alignItems: 'flex-end' },
  columnTheirs: { alignItems: 'stretch' },
  meta: { paddingHorizontal: 4 },
  bubble: { borderWidth: StyleSheet.hairlineWidth, borderColor: 'transparent', borderCurve: 'continuous' },
  text: { ...ramp.body },
  error: { flexDirection: 'row', alignItems: 'flex-start', gap: 6, paddingHorizontal: 8, paddingVertical: 6 },
  flex: { flex: 1 },
});
