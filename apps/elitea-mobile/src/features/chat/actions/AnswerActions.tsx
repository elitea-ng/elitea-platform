import { useQueryClient } from '@tanstack/react-query';
import * as Clipboard from 'expo-clipboard';
import { useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { clearFeedback, feedbackErrorText, setFeedback, useFeedback } from '@/api/queries/feedback';
import { queryKeys } from '@/api/queries/keys';
import { useDesign } from '@/design/DesignProvider';
import type { ActionPlan } from '@/features/chat/actions/messageActions';
import type { ChatMessage } from '@/features/chat/messageModel';
import { haptic, IconButton, type IconName } from '@/ui';

/** The action row's buttons: 36 pt visuals in 44/48 targets (§4.6). */
const BUTTON = 36;

export interface AnswerActionsProps {
  message: ChatMessage;
  /** What the workspace allows for this answer (`planActions`). */
  plan: ActionPlan;
  projectId: number | null;
  /** This answer is being read aloud. */
  reading: boolean;
  onReadAloud: () => void;
  onStopReading: () => void;
  onRegenerate: () => void;
  /** The full actions (the long-press menu): copy code, share, feedback with a comment. */
  onMore: () => void;
  /** Dislike asks for an optional reason in the actions sheet. */
  onDislike: () => void;
  /** A short confirmation ("Copied") or a problem, shown as a toast by the screen. */
  onNote: (note: string) => void;
  /**
   * Read the caller's rating from the server for this answer (the screen sets it on the newest
   * answer only). Every other row shows a rating it already knows (the cache: given here, or
   * read by the actions sheet) and asks for none, so a long thread does not fire one feedback
   * GET per rendered answer. Like and dislike never wait for the read.
   */
  loadRating?: boolean;
}

/**
 * The action row under a finished answer, as in the reference (docs/design-system.md §4.6):
 * copy, like, dislike and read aloud on the left, regenerate (newest answer only) and the
 * "…" menu on the right. Only what the workspace offers is drawn: copy needs
 * `allow_share_out`, like/dislike client contract 1.3 and a persisted answer, regenerate
 * the screen's own checks; never a dead button. Like and dislike are toggles
 * (`accessibilityState.selected`, filled glyph in the accent). At accessibility text sizes the
 * row collapses to the "…" menu, which offers the same actions as a list.
 */
export function AnswerActions({
  message,
  plan,
  projectId,
  reading,
  onReadAloud,
  onStopReading,
  onRegenerate,
  onMore,
  onDislike,
  onNote,
  loadRating = false,
}: AnswerActionsProps) {
  const design = useDesign();
  const { colors } = design;
  const queryClient = useQueryClient();
  const rateable = plan.actions.includes('like');
  const feedback = useFeedback(projectId, message.uuid, rateable && loadRating);
  const [saving, setSaving] = useState(false);
  const mine = feedback.data?.mine?.rating ?? null;

  const copy = async () => {
    try {
      await Clipboard.setStringAsync(message.text);
      haptic('copy');
      onNote('Copied');
    } catch {
      onNote("Couldn't copy.");
    }
  };
  const like = async () => {
    if (projectId === null || message.uuid === null || saving) return;
    haptic('feedbackLikeDislike');
    setSaving(true);
    try {
      // Tapping the rating you already gave retracts it.
      const next = mine === 1 ? await clearFeedback(projectId, message.uuid) : await setFeedback(projectId, message.uuid, 1, null);
      queryClient.setQueryData([...queryKeys.feedback(projectId), message.uuid], next);
      if (mine !== 1) onNote('Thanks for the feedback.');
    } catch (error) {
      onNote(feedbackErrorText(error));
    } finally {
      setSaving(false);
    }
  };
  const dislike = async () => {
    if (projectId === null || message.uuid === null || saving) return;
    haptic('feedbackLikeDislike');
    if (mine !== -1) {
      onDislike();
      return;
    }
    setSaving(true);
    try {
      queryClient.setQueryData([...queryKeys.feedback(projectId), message.uuid], await clearFeedback(projectId, message.uuid));
    } catch (error) {
      onNote(feedbackErrorText(error));
    } finally {
      setSaving(false);
    }
  };

  const button = (key: string, icon: IconName, label: string, onPress: () => void, extra: { selected?: boolean; disabled?: boolean; hint?: string } = {}) => (
    <IconButton
      key={key}
      icon={icon}
      variant="plain"
      size={BUTTON}
      iconSize={18}
      iconColor={extra.selected ? colors.accent : colors.textSecondary}
      selected={extra.selected}
      disabled={extra.disabled}
      accessibilityLabel={label}
      accessibilityHint={extra.hint}
      onPress={onPress}
    />
  );

  const more = button('more', 'more', 'Answer actions', onMore, { hint: 'Copy code, share, feedback and more' });
  if (design.accessibilityLayout) return <View style={styles.row}>{more}</View>;

  // The reference's order: copy, like, dislike, read aloud.
  const order: ActionPlan['actions'] = ['copy', 'like', 'dislike', 'read_aloud'];
  const left = order.filter((action) => plan.actions.includes(action)).flatMap((action) => {
    switch (action) {
      case 'copy':
        return [button('copy', 'copy', 'Copy answer', () => void copy())];
      case 'like':
        return [
          button('like', 'like', 'Like', () => void like(), {
            selected: mine === 1,
            disabled: saving,
            hint: mine === 1 ? 'Removes your rating' : undefined,
          }),
        ];
      case 'dislike':
        return [
          button('dislike', 'dislike', 'Dislike', () => void dislike(), {
            selected: mine === -1,
            disabled: saving,
            hint: mine === -1 ? 'Removes your rating' : 'Asks what was wrong',
          }),
        ];
      case 'read_aloud':
        return [
          reading
            ? button('read', 'speakerOff', 'Stop reading answer', onStopReading, { selected: true })
            : button('read', 'readAloud', 'Read answer aloud', onReadAloud),
        ];
      default:
        return [];
    }
  });
  return (
    <View style={styles.row} testID="answer-actions">
      <View style={styles.group}>{left}</View>
      <View style={styles.group}>
        {plan.actions.includes('regenerate') ? button('regenerate', 'regenerate', 'Regenerate', onRegenerate) : null}
        {more}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', marginTop: -2 },
  group: { flexDirection: 'row', alignItems: 'center', gap: 4 },
});
