import { useQueryClient } from '@tanstack/react-query';
import * as Clipboard from 'expo-clipboard';
import { useState } from 'react';
import { ActivityIndicator, Share, StyleSheet, TextInput, View } from 'react-native';

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
import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { ActionMenu, type ActionMenuItem } from '@/features/chat/ActionMenu';
import { codeBlockLabel, type ActionPlan, type MessageActionKey } from '@/features/chat/actions/messageActions';
import type { ChatMessage } from '@/features/chat/messageModel';
import { useShareOut } from '@/security/policy';
import { Button, haptic, ramp, Text, type IconName } from '@/ui';

export interface MessageActionsSheetProps {
  /** The message the sheet acts on; null hides it. */
  message: ChatMessage | null;
  plan: ActionPlan;
  projectId: number | null;
  onClose: () => void;
  onReadAloud?: (message: ChatMessage) => void;
  onRegenerate?: (message: ChatMessage) => void;
  /** Open straight on the dislike comment (the action row's Dislike). */
  startWithDislike?: boolean;
}

/**
 * The actions of one message, in a bottom sheet: copy (the whole message, or
 * one code block), share, read aloud, like / dislike with an optional
 * comment, regenerate. What is offered comes from `planActions`.
 */
export function MessageActionsSheet(props: MessageActionsSheetProps) {
  // Keyed by the message, so a sheet opened on another message starts fresh.
  return props.message ? <SheetBody key={`${props.message.key}:${props.startWithDislike ? 'd' : ''}`} {...props} message={props.message} /> : <ActionMenu visible={false} title="" items={[]} onClose={props.onClose} />;
}

const LABELS: Record<Exclude<MessageActionKey, 'copy_code' | 'like' | 'dislike'>, string> = {
  copy: 'Copy',
  share: 'Share…',
  read_aloud: 'Read aloud',
  regenerate: 'Regenerate answer',
};

const ICONS: Record<Exclude<MessageActionKey, 'like' | 'dislike'>, IconName> = {
  copy: 'copy',
  copy_code: 'code',
  share: 'share',
  read_aloud: 'readAloud',
  regenerate: 'regenerate',
};

/**
 * The long-press menu of a message (docs/design-system.md §4.6), a bottom sheet with SF Symbol
 * / Material Symbol rows: copy (the whole message, or one code block), share, read aloud,
 * regenerate; and, for a persisted answer on client contract 1.3, the rating (Good / Bad) with
 * an optional comment for a Bad one. What is offered comes from `planActions`.
 */
function SheetBody({ message, plan, projectId, onClose, onReadAloud, onRegenerate, startWithDislike = false }: MessageActionsSheetProps & { message: ChatMessage }) {
  const design = useDesign();
  const { colors, radius } = design;
  const queryClient = useQueryClient();
  const [note, setNote] = useState<string | null>(null);
  const rateable = plan.actions.includes('like');
  const feedback = useFeedback(projectId, message.uuid, rateable);
  const [summary, setSummary] = useState<FeedbackSummary | null>(null);
  const shown = summary ?? feedback.data ?? null;
  const [commentFor, setCommentFor] = useState<Rating | null>(startWithDislike && rateable ? -1 : null);
  const [comment, setComment] = useState('');
  const [saving, setSaving] = useState(false);
  // The comment field is prefilled with the stored feedback comment.
  const shareOut = useShareOut();

  const copy = async (text: string, what: string) => {
    try {
      await Clipboard.setStringAsync(text);
      haptic('copy');
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
    haptic('feedbackLikeDislike');
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

  const rows: ActionMenuItem[] = [];
  for (const action of plan.actions) {
    switch (action) {
      case 'copy':
        rows.push({ key: 'copy', label: LABELS.copy, icon: ICONS.copy, onPress: () => void copy(message.text, 'Message') });
        break;
      case 'copy_code':
        plan.codeBlocks.forEach((block, index) =>
          rows.push({
            key: `code-${index}`,
            label: codeBlockLabel(block, index, plan.codeBlocks.length),
            icon: ICONS.copy_code,
            onPress: () => void copy(block.code, 'Code'),
          }),
        );
        break;
      case 'share':
        rows.push({ key: 'share', label: LABELS.share, icon: ICONS.share, onPress: () => void share() });
        break;
      case 'read_aloud':
        if (onReadAloud) {
          rows.push({
            key: 'read_aloud',
            label: LABELS.read_aloud,
            icon: ICONS.read_aloud,
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
            icon: ICONS.regenerate,
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

  const header = (
    <>
      {plan.shareOutNote ? (
        <Text variant="subheadline" tone="secondary" maxFontSizeMultiplier={FONT_SCALE.body}>
          {plan.shareOutNote}
        </Text>
      ) : null}
      {rateable ? (
        <View style={styles.ratingRow}>
          {([1, -1] as Rating[]).map((rating) => {
            const active = shown?.mine?.rating === rating;
            return (
              <Button
                key={rating}
                title={rating === 1 ? 'Good' : 'Bad'}
                accessibilityLabel={rating === 1 ? 'Good answer' : 'Bad answer'}
                accessibilityHint={active ? 'Removes your rating' : undefined}
                accessibilityState={{ selected: active, disabled: saving || feedback.isLoading, busy: saving }}
                icon={rating === 1 ? 'like' : 'dislike'}
                variant={active ? 'filled' : 'tonal'}
                size="small"
                disabled={saving || feedback.isLoading}
                onPress={() => rate(rating)}
                style={styles.rating}
              />
            );
          })}
          {feedback.isLoading || saving ? <ActivityIndicator color={colors.accent} /> : null}
        </View>
      ) : null}
      {commentFor !== null ? (
        <View style={styles.comment}>
          <TextInput
            value={comment}
            onChangeText={setComment}
            placeholder="What was wrong? (optional)"
            placeholderTextColor={colors.textTertiary}
            accessibilityLabel="Feedback comment"
            contextMenuHidden={!shareOut}
            maxLength={MAX_FEEDBACK_COMMENT}
            multiline
            autoFocus
            maxFontSizeMultiplier={FONT_SCALE.input}
            style={[styles.input, { color: colors.textPrimary, borderColor: colors.separatorStrong, backgroundColor: colors.surfaceSunken, borderRadius: radius.sm }]}
          />
          <View style={styles.commentActions}>
            <Button title="Cancel" variant="plain" size="small" onPress={() => setCommentFor(null)} />
            <Button title="Send feedback" variant="filled" size="small" disabled={saving} onPress={() => void record(-1, comment)} />
          </View>
        </View>
      ) : null}
    </>
  );

  return (
    <ActionMenu visible title={message.role === 'assistant' ? 'Answer' : 'Message'} items={rows} onClose={onClose} header={header}>
      {note ? (
        <Text variant="subheadline" tone="secondary" accessibilityLiveRegion="polite" maxFontSizeMultiplier={FONT_SCALE.body}>
          {note}
        </Text>
      ) : null}
    </ActionMenu>
  );
}

const styles = StyleSheet.create({
  ratingRow: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 4 },
  rating: { flexGrow: 1 },
  comment: { gap: 6 },
  input: { ...ramp.subheadline, borderWidth: StyleSheet.hairlineWidth, minHeight: 64, maxHeight: 140, padding: 8 },
  commentActions: { flexDirection: 'row', justifyContent: 'flex-end', gap: 8 },
});
