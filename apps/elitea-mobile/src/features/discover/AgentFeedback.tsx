import { StyleSheet, View } from 'react-native';

import { ratingSentence, signalsSentence, type FeedbackItemView, type ListingFeedbackView } from '@/api/queries/marketplaceFeedback';
import { useDesign } from '@/design/DesignProvider';
import { ListSection, Text } from '@/ui';

function itemHead(item: FeedbackItemView): string {
  const who = item.isReport ? item.author || 'Workforce report from a hiring project' : `Review by ${item.author || 'a person'}`;
  const rated = item.rating === null ? null : `Rated ${item.rating} out of 5`;
  const outcome = item.outcome ? item.outcome.charAt(0).toUpperCase() + item.outcome.slice(1) : null;
  return [who, item.versionName ? `on ${item.versionName}` : null, rated, outcome].filter(Boolean).join(' · ');
}

/**
 * The feedback of the agent's Marketplace listing, read-only (ADR-0051 decision 5): the rating as
 * text, the signals beside it, and the newest reviews and workforce reports. Writing is on web and desktop.
 */
export function AgentFeedback({ feedback }: { feedback: ListingFeedbackView }) {
  const { colors, radius } = useDesign();
  return (
    <ListSection title="Feedback">
      <View testID="agent-feedback" style={[styles.box, { backgroundColor: colors.surfaceSunken, borderRadius: radius.lg }]}>
        <Text variant="callout" testID="agent-feedback-rating">
          {ratingSentence(feedback)}
        </Text>
        <Text variant="footnote" tone="secondary" testID="agent-feedback-signals">
          {signalsSentence(feedback)}
        </Text>
        {feedback.items.length === 0 ? (
          <Text variant="footnote" tone="secondary">
            No feedback yet.
          </Text>
        ) : (
          feedback.items.map((item) => (
            <View key={item.id} testID={`agent-feedback-item-${item.id}`} style={styles.item}>
              <Text variant="footnote" tone="secondary" emphasized>
                {itemHead(item)}
              </Text>
              {item.body ? <Text variant="callout">{item.body}</Text> : null}
            </View>
          ))
        )}
      </View>
    </ListSection>
  );
}

const styles = StyleSheet.create({
  box: { padding: 16, gap: 8, borderCurve: 'continuous' },
  item: { gap: 2, paddingTop: 8 },
});
