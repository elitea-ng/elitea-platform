import type { ReactElement } from 'react';
import { ActivityIndicator, ScrollView, StyleSheet, View, type RefreshControlProps } from 'react-native';

import { proposalTitle, type MarketplaceProposalView } from '@/api/queries/workforce';
import { useDesign } from '@/design/DesignProvider';
import { EmptyState, ListSection, Text, useLayoutMargin } from '@/ui';

export const WORKFORCE_HINT = 'Open the web or desktop app to hire or publish.';

export interface MarketplaceProposalsViewProps {
  status: { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string } | { kind: 'unavailable'; message: string };
  rows: readonly MarketplaceProposalView[];
  selectable: boolean;
  onRetry: () => void;
  refreshControl?: ReactElement<RefreshControlProps>;
}

/**
 * The open hire proposals and drafted reports of the project's workforce managers (ADR-0051
 * decision 6), read-only. What an agent wrote is plain text: nothing here is a link or markup.
 */
export function MarketplaceProposalsView({ status, rows, selectable, onRetry, refreshControl }: MarketplaceProposalsViewProps) {
  const { colors, radius } = useDesign();
  const margin = useLayoutMargin();

  if (status.kind === 'unavailable') {
    return (
      <View style={[styles.flex, { backgroundColor: colors.background }]}>
        <EmptyState mark="small" title="Workforce proposals aren't available" body={status.message} />
      </View>
    );
  }
  if (status.kind === 'loading') {
    return (
      <View style={[styles.flex, styles.center, { backgroundColor: colors.background }]}>
        <ActivityIndicator color={colors.accent} />
      </View>
    );
  }
  const content =
    rows.length === 0 ? (
      status.kind === 'error' ? (
        <EmptyState mark="none" title="Couldn't load the proposals" body={status.message} action={{ title: 'Try again', onPress: onRetry }} />
      ) : (
        <EmptyState title="No proposals" body={`No workforce manager has proposed a hire or drafted a report. ${WORKFORCE_HINT}`} />
      )
    ) : (
      <View style={[styles.sections, { paddingHorizontal: margin }]}>
        <ListSection title={rows.length === 1 ? '1 proposal' : `${rows.length} proposals`} footer={WORKFORCE_HINT}>
          {rows.map((proposal) => (
            <View
              key={proposal.id}
              testID={`workforce-proposal-${proposal.id}`}
              accessible
              accessibilityLabel={proposalTitle(proposal)}
              style={[styles.row, { backgroundColor: colors.surface, borderRadius: radius.md }]}>
              <Text variant="headline">{proposalTitle(proposal)}</Text>
              {proposal.text ? (
                <Text variant="callout" tone="secondary" selectable={selectable}>
                  {proposal.text}
                </Text>
              ) : null}
            </View>
          ))}
        </ListSection>
      </View>
    );
  return (
    <ScrollView contentInsetAdjustmentBehavior="automatic" style={{ backgroundColor: colors.background }} refreshControl={refreshControl}>
      {content}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { alignItems: 'center', justifyContent: 'center' },
  sections: { paddingVertical: 16, gap: 24 },
  row: { padding: 16, gap: 4 },
});
