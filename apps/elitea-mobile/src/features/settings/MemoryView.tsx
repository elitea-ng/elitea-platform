import type { ReactElement } from 'react';
import { ActivityIndicator, ScrollView, StyleSheet, View, type RefreshControlProps } from 'react-native';

import type { Memory } from '@/api/queries/memories';
import { SwipeToDelete } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import { Button, EmptyState, Icon, ListRow, ListSection, Text, useLayoutMargin } from '@/ui';

export interface MemoryViewProps {
  /** `unavailable` carries why (an older workspace, or not known yet while offline). */
  status: { kind: 'loading' } | { kind: 'ready' } | { kind: 'error'; message: string } | { kind: 'unavailable'; message: string };
  rows: readonly Memory[];
  /** A failed refresh, delete or clear, shown above the rows that are still there. */
  banner: string | null;
  /** The workspace allows copying text out (share-out policy). */
  selectable: boolean;
  /** Deletes one memory; resolves false when the server refused (the row slides back). */
  onDelete: (memory: Memory) => Promise<boolean>;
  onClearAll: () => void;
  onRetry: () => void;
  refreshControl?: ReactElement<RefreshControlProps>;
}

/** A memory's spoken label: its text, "turned off" when it is, and its tags. */
export function memoryLabel(memory: Pick<Memory, 'content' | 'enabled' | 'tags'>): string {
  return [memory.content, memory.enabled ? null : 'turned off', memory.tags.length > 0 ? `tags ${memory.tags.join(', ')}` : null].filter(Boolean).join(', ');
}

/**
 * Settings → Memory (client contract 1.3): what the workspace recalls about the caller in this
 * project, as one inset-grouped section; each row has a visible Delete button, and swiping it left
 * is the shortcut (also its accessibility action, S9); "Clear all memories" is a destructive row of its own at the end (it confirms first).
 * The mascot appears only on the empty state, never on an error. The route and the `__DEV__`
 * preview both render this (D2).
 */
export function MemoryView({ status, rows, banner, selectable, onDelete, onClearAll, onRetry, refreshControl }: MemoryViewProps) {
  const { colors, radius } = useDesign();
  const margin = useLayoutMargin();

  if (status.kind === 'unavailable') {
    return (
      <View style={[styles.flex, { backgroundColor: colors.background }]}>
        <EmptyState mascot="bust" title="Memory isn't available" body={status.message} />
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
        <EmptyState mascot="none" title="Couldn't load your memories" body={status.message} action={{ title: 'Try again', onPress: onRetry }} />
      ) : (
        <EmptyState
          title="Nothing remembered yet"
          body="When you ask an agent to remember something about you, it appears here. Only you can see your memories."
        />
      )
    ) : (
      <View style={[styles.sections, { paddingHorizontal: margin }]}>
        <ListSection
          title={rows.length === 1 ? '1 memory' : `${rows.length} memories`}
          footer="What the workspace recalls before it answers you in this project.">
          {rows.map((memory) => (
            <SwipeToDelete key={memory.id} actionLabel="Delete" onDelete={() => onDelete(memory)} testID={`memory-${memory.id}`}>
              {(a11y, remove) => (
                <View style={[styles.row, { backgroundColor: colors.surface }]}>
                  <View {...a11y} accessible accessibilityLabel={memoryLabel(memory)} style={styles.memoryText}>
                    <Text variant="body" selectable={selectable} tone={memory.enabled ? 'primary' : 'secondary'}>
                      {memory.content}
                    </Text>
                    {memory.tags.length > 0 || !memory.enabled ? (
                      <View style={styles.meta}>
                        {!memory.enabled ? (
                          <View style={[styles.badge, { backgroundColor: colors.warningSoft, borderRadius: radius.pill }]}>
                            <Icon name="pauseCircle" size={12} tone="warning" />
                            <Text variant="caption" tone="warning">
                              Off
                            </Text>
                          </View>
                        ) : null}
                        {memory.tags.map((tag) => (
                          <Text key={tag} variant="caption" tone="secondary">
                            #{tag}
                          </Text>
                        ))}
                      </View>
                    ) : null}
                  </View>
                  {/* The visible way to delete one (S9); its own element, outside the group. */}
                  <Button
                    title="Delete"
                    variant="destructivePlain"
                    size="small"
                    accessibilityLabel={`Delete memory: ${memory.content.slice(0, 80)}`}
                    loading={a11y.accessibilityState?.busy === true}
                    onPress={remove}
                    style={styles.end}
                  />
                </View>
              )}
            </SwipeToDelete>
          ))}
        </ListSection>
        <ListSection>
          <ListRow title="Clear all memories" destructive accessibilityHint="Asks first, then deletes every memory in this project" onPress={onClearAll} />
        </ListSection>
      </View>
    );

  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={rows.length === 0 ? styles.grow : styles.content}
      refreshControl={refreshControl}>
      {banner ? (
        <View
          accessible
          accessibilityRole="alert"
          style={[styles.banner, { marginHorizontal: margin, backgroundColor: colors.dangerSoft, borderRadius: radius.md }]}>
          <Icon name="error" size={18} tone="danger" />
          <Text variant="callout" tone="danger" style={styles.flex}>
            {banner}
          </Text>
        </View>
      ) : null}
      {content}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { alignItems: 'center', justifyContent: 'center' },
  grow: { flexGrow: 1 },
  content: { paddingVertical: 16, paddingBottom: 40 },
  sections: { gap: 28 },
  row: { paddingHorizontal: 16, paddingTop: 12, paddingBottom: 4, gap: 2 },
  memoryText: { gap: 6 },
  end: { alignSelf: 'flex-end' },
  meta: { flexDirection: 'row', flexWrap: 'wrap', alignItems: 'center', gap: 8 },
  badge: { flexDirection: 'row', alignItems: 'center', gap: 4, paddingHorizontal: 8, paddingVertical: 2 },
  banner: { flexDirection: 'row', alignItems: 'center', gap: 10, padding: 12, marginBottom: 16, borderCurve: 'continuous' },
});
