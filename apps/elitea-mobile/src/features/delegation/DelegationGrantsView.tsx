import type { ReactElement } from 'react';
import { ActivityIndicator, ScrollView, StyleSheet, View, type RefreshControlProps } from 'react-native';

import { canApprove, canReapprove, canRevoke, stateLabel, type DelegationGrantView, type DelegationItem } from '@/api/queries/delegation';
import { useDesign } from '@/design/DesignProvider';
import { Button, EmptyState, ListSection, Text, useLayoutMargin } from '@/ui';

export const DELEGATION_HINT = 'Attaching an agent to another one is done on the web or desktop app.';
export const REAPPROVAL_GOVERNED_NOTE = 'Re-approval is decided in a change request on the web or desktop app. Until then the agent runs without the new items.';
export const GOVERNED_NOTE = 'Awaiting approval, decided in a change request on the web or desktop app.';

export interface DelegationGrantsViewProps {
  agentName: string;
  status: { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string } | { kind: 'unavailable'; message: string };
  grants: readonly DelegationGrantView[];
  /** Application id to name, for the callees the app knows. */
  names: Readonly<Record<number, string>>;
  /** The grant whose approve or revoke is running. */
  busy: number | null;
  /** A failed refresh, approve or revoke, shown above the rows. */
  banner: string | null;
  selectable: boolean;
  onApprove: (grant: DelegationGrantView) => void;
  onRevoke: (grant: DelegationGrantView) => void;
  onRetry: () => void;
  refreshControl?: ReactElement<RefreshControlProps>;
}

const EDGE: Record<string, string> = {
  sub_agent: 'Sub-agent',
  agent_tool: 'Agent used as a tool',
  pipeline_child: 'Pipeline child',
  pipeline_node: 'Pipeline node',
};
const KIND: Record<string, string> = {
  toolkit: 'Toolkit',
  tool: 'Tool',
  credential: 'Credential reference',
  data_class: 'Data class',
  egress_host: 'Egress host',
};

export const calleeName = (grant: Pick<DelegationGrantView, 'calleeApplicationId'>, names: Readonly<Record<number, string>>): string =>
  names[grant.calleeApplicationId] ?? `Agent ${grant.calleeApplicationId}`;

/** "Toolkit toolkit:7", "Tool git_push (toolkit:7)". Plain text: what the server listed, as listed. */
export function itemLine(item: DelegationItem): string {
  const kind = KIND[item.kind] ?? item.kind;
  return item.toolkit ? `${kind} ${item.name} (${item.toolkit})` : `${kind} ${item.name}`;
}

/**
 * The delegation grants of one agent (ADR-0044 amendment 1): what each callee agent was attached
 * with, its state, and, for an editor, Approve and Revoke. The app has no agent editor, so
 * attaching is on the web and desktop. A grant under the governed profile is decided in its
 * change request there, so no Approve is offered for it.
 */
export function DelegationGrantsView({ agentName, status, grants, names, busy, banner, selectable, onApprove, onRevoke, onRetry, refreshControl }: DelegationGrantsViewProps) {
  const { colors, radius } = useDesign();
  const margin = useLayoutMargin();

  if (status.kind === 'unavailable') {
    return (
      <View style={[styles.flex, { backgroundColor: colors.background }]}>
        <EmptyState mark="small" title="Delegations aren't available" body={status.message} />
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
    grants.length === 0 ? (
      status.kind === 'error' ? (
        <EmptyState mark="none" title="Couldn't load the delegations" body={status.message} action={{ title: 'Try again', onPress: onRetry }} />
      ) : (
        <EmptyState title="No delegations" body={`${agentName} has no agents attached that bring tools of their own. ${DELEGATION_HINT}`} />
      )
    ) : (
      <View style={[styles.sections, { paddingHorizontal: margin }]}>
        {banner ? (
          <Text variant="footnote" tone="danger" accessibilityRole="alert">
            {banner}
          </Text>
        ) : null}
        <ListSection title={grants.length === 1 ? '1 delegation' : `${grants.length} delegations`} footer={DELEGATION_HINT}>
          {grants.map((grant) => {
            const name = calleeName(grant, names);
            const label = stateLabel(grant);
            const running = busy === grant.id;
            return (
              <View
                key={grant.id}
                testID={`delegation-grant-${grant.id}`}
                style={[styles.row, { backgroundColor: colors.surface, borderRadius: radius.md }]}>
                <Text variant="headline" heading>
                  {name}
                </Text>
                <Text variant="footnote" tone="secondary">
                  {`${EDGE[grant.edgeKind] ?? grant.edgeKind} · ${label}${grant.legacy ? ' · From before delegations' : ''}`}
                </Text>
                {grant.state === 'pending' && grant.governed ? (
                  <Text variant="callout" tone="secondary">
                    {GOVERNED_NOTE}
                  </Text>
                ) : null}
                {grant.state === 'stale' && grant.governed ? (
                  <Text variant="callout" tone="secondary">
                    {REAPPROVAL_GOVERNED_NOTE}
                  </Text>
                ) : null}
                <Text variant="subheadline" tone="secondary">
                  {grant.state === 'pending' ? `${name} would be able to use` : `${name} can use`}
                </Text>
                {grant.items.length === 0 ? (
                  <Text variant="callout" tone="secondary">
                    Nothing yet.
                  </Text>
                ) : (
                  grant.items.map((item) => (
                    <Text key={`${item.kind}:${item.name}:${item.toolkit ?? ''}`} variant="callout" selectable={selectable}>
                      {itemLine(item)}
                    </Text>
                  ))
                )}
                {grant.state === 'stale' && grant.drift.length > 0 ? (
                  <>
                    <Text variant="subheadline" tone="secondary">
                      {`${name} now also holds, not approved yet`}
                    </Text>
                    {grant.drift.map((item) => (
                      <Text key={`drift:${item.kind}:${item.name}:${item.toolkit ?? ''}`} variant="callout" tone="danger" selectable={selectable}>
                        {itemLine(item)}
                      </Text>
                    ))}
                  </>
                ) : null}
                {grant.omitted.length > 0 ? (
                  <>
                    <Text variant="subheadline" tone="secondary">
                      Left out, because nobody who approved holds access to it
                    </Text>
                    {grant.omitted.map((item) => (
                      <Text key={`omitted:${item.kind}:${item.name}:${item.toolkit ?? ''}`} variant="callout" tone="secondary" selectable={selectable}>
                        {itemLine(item)}
                      </Text>
                    ))}
                  </>
                ) : null}
                {canApprove(grant) || canReapprove(grant) || canRevoke(grant) ? (
                  <View style={styles.actions}>
                    {canApprove(grant) ? (
                      <Button
                        title="Approve"
                        size="small"
                        accessibilityLabel={`Approve delegation to ${name}`}
                        disabled={busy !== null}
                        loading={running}
                        onPress={() => onApprove(grant)}
                      />
                    ) : null}
                    {canReapprove(grant) ? (
                      <Button
                        title="Re-approve"
                        size="small"
                        accessibilityLabel={`Re-approve delegation to ${name}`}
                        disabled={busy !== null}
                        loading={running}
                        onPress={() => onApprove(grant)}
                      />
                    ) : null}
                    {canRevoke(grant) ? (
                      <Button
                        title="Revoke"
                        variant="destructivePlain"
                        size="small"
                        accessibilityLabel={`Revoke delegation to ${name}`}
                        disabled={busy !== null}
                        onPress={() => onRevoke(grant)}
                      />
                    ) : null}
                  </View>
                ) : null}
              </View>
            );
          })}
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
  sections: { paddingVertical: 16, gap: 16 },
  row: { padding: 16, gap: 6 },
  actions: { flexDirection: 'row', gap: 8, paddingTop: 8 },
});
