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

/** The action row's buttons: 32 pt visuals in 44/48 targets (§4.6); the glyphs small and outlined. */
const BUTTON = 32;
const GLYPH = 16;

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
 * one compact, quiet row below the bubble, left-aligned: copy, like, dislike, read aloud,
 * regenerate (newest answer only), then the "…" menu with the rest (copy code, share, fork).
 * The screen draws it under the latest answer; an older one shows it on a long press (the
 * actions sheet) or while a screen reader runs. Only what the workspace offers is drawn: copy needs
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
      iconSize={GLYPH}
      iconColor={extra.selected ? colors.accent : colors.textTertiary}
      selected={extra.selected}
      disabled={extra.disabled}
      accessibilityLabel={label}
      accessibilityHint={extra.hint}
      onPress={onPress}
    />
  );

  const more = button('more', 'more', 'Answer actions', onMore, { hint: 'Copy code, share, feedback and more' });
  if (design.accessibilityLayout) {
    return (
      <View style={styles.row} testID="answer-actions">
        {more}
      </View>
    );
  }

  // The reference's order: copy, like, dislike, read aloud, regenerate, then "…".
  const order: ActionPlan['actions'] = ['copy', 'like', 'dislike', 'read_aloud', 'regenerate'];
  const buttons = order.filter((action) => plan.actions.includes(action)).flatMap((action) => {
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
      case 'regenerate':
        return [button('regenerate', 'regenerate', 'Regenerate', onRegenerate)];
      default:
        return [];
    }
  });
  return (
    <View style={styles.row} testID="answer-actions">
      {buttons}
      {more}
    </View>
  );
}

const styles = StyleSheet.create({
  // The 44 pt frames touch; the small glyphs inside them keep the row quiet. Pulled in so the
  // first glyph lines up with the bubble's text rather than its frame.
  row: { flexDirection: 'row', alignItems: 'center', justifyContent: 'flex-start', flexWrap: 'wrap', gap: 0, marginTop: -6, marginStart: -6 },
});
