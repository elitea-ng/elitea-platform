import { useQueryClient } from '@tanstack/react-query';
import * as Clipboard from 'expo-clipboard';
import { useState } from 'react';
import { ActivityIndicator, Pressable, Share, StyleSheet, Text, TextInput, View } from 'react-native';

import {
  MAX_FEEDBACK_COMMENT,
  clearFeedback,
  feedbackErrorText,
  setFeedback,
  useFeedback,
  type FeedbackSummary,
  type Rating,
} from '@/api/queries/feedback';
import { queryKeys } from '@/api/queries/keys';
import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { useModalAnimation } from '@/features/a11y/motion';
import { codeBlockLabel, type ActionPlan, type MessageActionKey } from '@/features/chat/actions/messageActions';
import type { ChatMessage } from '@/features/chat/messageModel';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { useShareOut } from '@/security/policy';
import { ramp } from '@/ui';

export interface MessageActionsSheetProps {
  /** The message the sheet acts on; null hides it. */
  message: ChatMessage | null;
  plan: ActionPlan;
  projectId: number | null;
  onClose: () => void;
  onReadAloud?: (message: ChatMessage) => void;
  onRegenerate?: (message: ChatMessage) => void;
}

/**
 * The actions of one message, in a bottom sheet: copy (the whole message, or
 * one code block), share, read aloud, like / dislike with an optional
 * comment, regenerate. What is offered comes from `planActions`.
 */
export function MessageActionsSheet(props: MessageActionsSheetProps) {
  const animation = useModalAnimation('slide');
  return (
    <LockAwareModal visible={props.message !== null} transparent animationType={animation} onRequestClose={props.onClose}>
      {props.message ? <SheetBody {...props} message={props.message} /> : null}
    </LockAwareModal>
  );
}

const LABELS: Record<Exclude<MessageActionKey, 'copy_code' | 'like' | 'dislike'>, string> = {
  copy: 'Copy',
  share: 'Share…',
  read_aloud: 'Read aloud',
  regenerate: 'Regenerate answer',
};

function SheetBody({ message, plan, projectId, onClose, onReadAloud, onRegenerate }: MessageActionsSheetProps & { message: ChatMessage }) {
  const palette = usePalette();
  const queryClient = useQueryClient();
  const [note, setNote] = useState<string | null>(null);
  const rateable = plan.actions.includes('like');
  const feedback = useFeedback(projectId, message.uuid, rateable);
  const [summary, setSummary] = useState<FeedbackSummary | null>(null);
  const shown = summary ?? feedback.data ?? null;
  const [commentFor, setCommentFor] = useState<Rating | null>(null);
  const [comment, setComment] = useState('');
  const [saving, setSaving] = useState(false);
  // The comment field is prefilled with the stored feedback comment.
  const shareOut = useShareOut();

  const copy = async (text: string, what: string) => {
    try {
      await Clipboard.setStringAsync(text);
      setNote(`${what} copied.`);
    } catch {
      setNote("Couldn't copy.");
    }
  };
  const share = async () => {
    try {
      await Share.share({ message: message.text });
      onClose();
    } catch {
      setNote("Couldn't open the share sheet.");
    }
  };
  const record = async (rating: Rating | null, text: string | null) => {
    if (projectId === null || message.uuid === null || saving) return;
    setSaving(true);
    setNote(null);
    try {
      const next = rating === null ? await clearFeedback(projectId, message.uuid) : await setFeedback(projectId, message.uuid, rating, text);
      setSummary(next);
      queryClient.setQueryData([...queryKeys.feedback(projectId), message.uuid], next);
      setCommentFor(null);
      setComment('');
      setNote(rating === null ? 'Feedback removed.' : rating === 1 ? 'Thanks for the feedback.' : 'Thanks. Your feedback was sent.');
    } catch (error) {
      setNote(feedbackErrorText(error));
    } finally {
      setSaving(false);
    }
  };
  const rate = (rating: Rating) => {
    // Tapping the rating you already gave retracts it.
    if (shown?.mine?.rating === rating && commentFor === null) {
      void record(null, null);
      return;
    }
    if (rating === -1) {
      setCommentFor(-1);
      setComment(shown?.mine?.rating === -1 ? (shown.mine.comment ?? '') : '');
      return;
    }
    void record(1, null);
  };

  const rows: { key: string; label: string; onPress: () => void }[] = [];
  for (const action of plan.actions) {
    switch (action) {
      case 'copy':
        rows.push({ key: 'copy', label: LABELS.copy, onPress: () => void copy(message.text, 'Message') });
        break;
      case 'copy_code':
        plan.codeBlocks.forEach((block, index) =>
          rows.push({
            key: `code-${index}`,
            label: codeBlockLabel(block, index, plan.codeBlocks.length),
            onPress: () => void copy(block.code, 'Code'),
          }),
        );
        break;
      case 'share':
        rows.push({ key: 'share', label: LABELS.share, onPress: () => void share() });
        break;
      case 'read_aloud':
        if (onReadAloud) {
          rows.push({
            key: 'read_aloud',
            label: LABELS.read_aloud,
            onPress: () => {
              onReadAloud(message);
              onClose();
            },
          });
        }
        break;
      case 'regenerate':
        if (onRegenerate) {
          rows.push({
            key: 'regenerate',
            label: LABELS.regenerate,
            onPress: () => {
              onRegenerate(message);
              onClose();
            },
          });
        }
        break;
      default:
        break;
    }
  }

  return (
    <View style={styles.backdrop}>
      <Pressable accessibilityLabel="Close message actions" accessibilityRole="button" style={styles.flex} onPress={onClose} />
      <View accessibilityViewIsModal style={[styles.sheet, { backgroundColor: palette.background, borderColor: palette.border }]}>
        <Text accessibilityRole="header" maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.title, { color: palette.textSecondary }]}>
          {message.role === 'assistant' ? 'Answer' : 'Message'}
        </Text>
        {plan.shareOutNote ? (
          <Text maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.note, { color: palette.textSecondary }]}>
            {plan.shareOutNote}
          </Text>
        ) : null}
        {rateable ? (
          <View style={styles.ratingRow}>
            {([1, -1] as Rating[]).map((rating) => {
              const active = shown?.mine?.rating === rating;
              const label = rating === 1 ? 'Good answer' : 'Bad answer';
              return (
                <Pressable
                  key={rating}
                  accessibilityRole="button"
                  accessibilityLabel={label}
                  accessibilityHint={active ? 'Removes your rating' : undefined}
                  accessibilityState={{ selected: active, disabled: saving || feedback.isLoading, busy: saving }}
                  disabled={saving || feedback.isLoading}
                  onPress={() => rate(rating)}
                  style={[
                    styles.rating,
                    { borderColor: active ? palette.accent : palette.border, backgroundColor: active ? palette.backgroundElement : 'transparent' },
                  ]}>
                  <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.ratingText, { color: active ? palette.accent : palette.text }]}>
                    {rating === 1 ? '👍 Good' : '👎 Bad'}
                  </Text>
                </Pressable>
              );
            })}
            {feedback.isLoading || saving ? <ActivityIndicator color={palette.accent} /> : null}
          </View>
        ) : null}
        {commentFor !== null ? (
          <View style={styles.comment}>
            <TextInput
              value={comment}
              onChangeText={setComment}
              placeholder="What was wrong? (optional)"
              placeholderTextColor={palette.textSecondary}
              accessibilityLabel="Feedback comment"
              contextMenuHidden={!shareOut}
              maxLength={MAX_FEEDBACK_COMMENT}
              multiline
              autoFocus
              maxFontSizeMultiplier={FONT_SCALE.input}
              style={[styles.input, { color: palette.text, borderColor: palette.inputBorder, backgroundColor: palette.field }]}
            />
            <View style={styles.commentActions}>
              <Pressable accessibilityRole="button" onPress={() => setCommentFor(null)} hitSlop={8}>
                <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.link, { color: palette.textSecondary }]}>
                  Cancel
                </Text>
              </Pressable>
              <Pressable
                accessibilityRole="button"
                accessibilityState={{ disabled: saving }}
                disabled={saving}
                onPress={() => void record(-1, comment)}
                hitSlop={8}>
                <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.link, { color: palette.accent }]}>
                  Send feedback
                </Text>
              </Pressable>
            </View>
          </View>
        ) : null}
        {rows.map((row) => (
          <Pressable
            key={row.key}
            accessibilityRole="button"
            onPress={row.onPress}
            style={({ pressed }) => [styles.row, { borderTopColor: palette.border }, pressed ? { backgroundColor: palette.backgroundElement } : null]}>
            <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.rowText, { color: palette.text }]}>
              {row.label}
            </Text>
          </Pressable>
        ))}
        {note ? (
          <Text accessibilityLiveRegion="polite" maxFontSizeMultiplier={FONT_SCALE.body} style={[styles.note, { color: palette.textSecondary }]}>
            {note}
          </Text>
        ) : null}
        <Pressable accessibilityRole="button" onPress={onClose} style={[styles.row, { borderTopColor: palette.border }]}>
          <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.rowText, styles.done, { color: palette.accent }]}>
            Done
          </Text>
        </Pressable>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  backdrop: { flex: 1, backgroundColor: 'rgba(0,0,0,0.35)' },
  sheet: {
    borderTopLeftRadius: 16,
    borderTopRightRadius: 16,
    borderWidth: StyleSheet.hairlineWidth,
    paddingTop: Spacing.three,
    paddingBottom: Spacing.five,
    paddingHorizontal: Spacing.three,
    gap: Spacing.two,
  },
  title: { ...ramp.footnote, fontWeight: '600', textTransform: 'uppercase' },
  note: { ...ramp.subheadline },
  ratingRow: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two },
  rating: { borderWidth: 1, borderRadius: 18, paddingHorizontal: Spacing.three, paddingVertical: Spacing.two, minHeight: 44, justifyContent: 'center' },
  ratingText: { ...ramp.subheadline, fontWeight: '600' },
  comment: { gap: Spacing.one },
  input: { ...ramp.subheadline, borderWidth: StyleSheet.hairlineWidth, borderRadius: 10, minHeight: 64, maxHeight: 140, padding: Spacing.two },
  commentActions: { flexDirection: 'row', justifyContent: 'flex-end', gap: Spacing.four },
  link: { ...ramp.subheadline, fontWeight: '600' },
  row: { minHeight: 48, justifyContent: 'center', borderTopWidth: StyleSheet.hairlineWidth },
  rowText: { ...ramp.body },
  done: { fontWeight: '600', textAlign: 'center' },
});
