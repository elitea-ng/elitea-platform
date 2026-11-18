import { router, type Href } from 'expo-router';
import { StyleSheet, View } from 'react-native';

import { offersReview, type DelegationNotice } from '@elitea/client-core/stream';

import { useDesign } from '@/design/DesignProvider';
import { delegationAskText, delegationNoticeText, delegationReviewHref } from '@/features/chat/delegationNotices';
import { Button, GlassSurface, Icon, Text } from '@/ui';

/**
 * One card per refused or narrowed delegation of an answer, under its tool
 * activity. "Review delegation" opens the approve screen only when the server
 * said the viewer may review; otherwise the card names who to ask.
 */
export function DelegationNoticeCards({ notices }: { notices: readonly DelegationNotice[] }) {
  const design = useDesign();
  if (notices.length === 0) return null;
  return (
    <View style={styles.list}>
      {notices.map((notice) => {
        const callerId = offersReview(notice) ? notice.callerId : undefined;
        const ask = notice.canReview === false && notice.reason !== 'principal' && notice.reason !== 'depth' ? delegationAskText(notice) : null;
        return (
          <GlassSurface key={notice.id} radius={design.radius.md} style={styles.card} testID={`delegation-notice-${notice.id}`}>
            <View style={styles.row}>
              <Icon name="info" size={18} tone="secondary" />
              <Text variant="footnote" tone="secondary" style={styles.flex}>
                {delegationNoticeText(notice)}
              </Text>
            </View>
            {ask !== null ? (
              <Text variant="footnote" tone="secondary" testID={`delegation-notice-ask-${notice.id}`}>
                {ask}
              </Text>
            ) : null}
            {callerId !== undefined ? (
              <View style={styles.actions}>
                <Button title="Review delegation" variant="tonal" size="small" onPress={() => router.push(delegationReviewHref(callerId) as Href)} />
              </View>
            ) : null}
          </GlassSurface>
        );
      })}
    </View>
  );
}

const styles = StyleSheet.create({
  list: { alignSelf: 'stretch', gap: 6, paddingTop: 6 },
  card: { alignSelf: 'stretch', gap: 6, paddingHorizontal: 12, paddingVertical: 10 },
  row: { flexDirection: 'row', alignItems: 'flex-start', gap: 8 },
  flex: { flex: 1 },
  actions: { flexDirection: 'row', justifyContent: 'flex-end' },
});
