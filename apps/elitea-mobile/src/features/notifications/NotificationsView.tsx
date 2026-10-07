import { useMemo, type ReactElement } from 'react';
import { ActivityIndicator, FlatList, Pressable, ScrollView, StyleSheet, View, type RefreshControlProps } from 'react-native';

import type { Notification } from '@/api/queries/notifications';
import { SwipeToDelete, type SwipeRowA11y } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import { groupChatsByDate } from '@/features/chats/sections';
import { describeNotification, notificationLabel, type FilterOption, type NotificationFilter } from '@/features/notifications/notificationModel';
import { Chip, EmptyState, hitTarget, Icon, IconTile, ListSection, pressedOverlay, ripple, Text, uiPlatform, useLayoutMargin } from '@/ui';

/** What the inbox needs of one notification. */
export interface InboxRow {
  id: number;
  event_type: string;
  meta: Notification['meta'];
  is_seen: boolean;
  created_at: string;
  /** Its age as shown ("5m", "2d"). */
  age: string;
}

export interface NotificationsViewProps {
  rows: readonly InboxRow[];
  /** "Today", "Yesterday" and so on are counted from this (tests and previews fix it). */
  now?: number;
  status: 'loading' | 'error' | 'ready';
  filters: readonly FilterOption[];
  filter: NotificationFilter;
  onFilter: (next: NotificationFilter) => void;
  /** Opens the conversation a row names (mentions). */
  onOpen: (row: InboxRow, conversationRef: string) => void;
  /** Dismisses a row (client contract 1.3, live list only); null: rows cannot be swiped. */
  onDismiss: ((row: InboxRow) => Promise<boolean>) | null;
  onRetry: () => void;
  /** Above the list (the offline notice). */
  header?: ReactElement;
  refreshControl?: ReactElement<RefreshControlProps>;
}

/**
 * The notifications inbox (docs/screens.md §7): inset-grouped sections by local day (Today,
 * Yesterday, Previous 7 days, Earlier), each row with its type symbol in a tinted circle, the
 * title (emphasized while unread, with an accent dot AND "unread" in its label, never colour
 * alone), the server's one-line detail and the age. A row that opens a conversation has the
 * link role and a chevron; one that does not has no press state at all (X12). Swipe left
 * dismisses (also the row's accessibility action). Type filter chips sit above the list.
 * "Mark all as read" is the navigation bar's action (the route sets it). The route and the
 * `__DEV__` preview both render this (D2).
 */
export function NotificationsView({
  rows,
  now,
  status,
  filters,
  filter,
  onFilter,
  onOpen,
  onDismiss,
  onRetry,
  header,
  refreshControl,
}: NotificationsViewProps) {
  const { colors } = useDesign();
  const margin = useLayoutMargin();
  const sections = useMemo(
    () =>
      groupChatsByDate(
        rows.map((row) => ({ row, updated_at: row.created_at })),
        now,
      ),
    [rows, now],
  );

  const chips =
    filters.length > 0 ? (
      <ScrollView
        testID="notification-filters"
        horizontal
        showsHorizontalScrollIndicator={false}
        // A ScrollView grows by default: without this the chip bar takes half the screen.
        style={styles.chipBar}
        contentContainerStyle={[styles.chips, { paddingHorizontal: margin }]}
        accessibilityRole="tablist">
        {filters.map((option) => (
          <Chip
            key={option.value}
            testID={`filter-${option.value}`}
            accessibilityRole="tab"
            label={option.label}
            accessibilityLabel={`${option.label}, ${option.count}`}
            selected={option.value === filter}
            onPress={() => onFilter(option.value)}
          />
        ))}
      </ScrollView>
    ) : null;

  const empty =
    status === 'loading' ? (
      <ActivityIndicator style={styles.loading} color={colors.accent} />
    ) : status === 'error' ? (
      <EmptyState
        mascot="none"
        title="Couldn't load notifications"
        body="Check your connection and try again."
        action={{ title: 'Try again', onPress: onRetry }}
      />
    ) : filter !== 'all' ? (
      <EmptyState mascot="none" title="Nothing of this kind" body="Choose All to see every notification." />
    ) : (
      <EmptyState mascot="bust" title="You're all caught up" body="Mentions and updates from your workspace appear here." />
    );

  return (
    <FlatList
      testID="notifications-list"
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={sections.length === 0 ? styles.grow : styles.list}
      data={status === 'ready' ? sections : []}
      keyExtractor={(section) => section.key}
      refreshControl={refreshControl}
      ListHeaderComponent={
        <>
          {header}
          {chips}
        </>
      }
      ListEmptyComponent={empty}
      renderItem={({ item: section }) => (
        <ListSection title={section.title} style={[styles.section, { marginHorizontal: margin }]}>
          {section.data.map(({ row }) => (
            <SwipeToDelete key={row.id} actionLabel="Dismiss" onDelete={onDismiss ? () => onDismiss(row) : null} testID={`notification-${row.id}`}>
              {(a11y) => <NotificationRow row={row} a11y={a11y} onOpen={onOpen} />}
            </SwipeToDelete>
          ))}
        </ListSection>
      )}
    />
  );
}

/** One inbox row; see `NotificationsView`. */
export function NotificationRow({ row, a11y, onOpen }: { row: InboxRow; a11y?: SwipeRowA11y; onOpen: (row: InboxRow, ref: string) => void }) {
  const design = useDesign();
  const { colors } = design;
  const view = describeNotification(row);
  const unread = !row.is_seen;
  const ref = view.conversationRef;
  const label = notificationLabel(view, row.age, unread);
  const leading = (
    <View style={styles.leading}>
      <View style={[styles.dot, { backgroundColor: unread ? colors.accent : 'transparent' }]} />
      <IconTile icon={view.icon} size={36} tone={view.icon === 'mention' ? 'accent' : 'secondary'} />
    </View>
  );
  const chevron = ref !== null && uiPlatform === 'ios' ? <Icon name="chevronRight" size={14} tone="tertiary" weight="semibold" /> : null;
  const age = (
    <Text variant="footnote" tone="secondary">
      {row.age}
    </Text>
  );
  const title = (
    <Text variant="body" emphasized={unread} numberOfLines={design.accessibilityLayout ? undefined : 2}>
      {view.title}
    </Text>
  );
  const detail = view.detail ? (
    <Text variant="footnote" tone="secondary" numberOfLines={design.accessibilityLayout ? undefined : 2}>
      {view.detail}
    </Text>
  ) : null;
  const pressedStyle = (pressed: boolean) => ({ minHeight: Math.max(hitTarget(), 60), backgroundColor: pressed ? pressedOverlay(design) : undefined });
  // At accessibility sizes the row stacks (S2): the symbol, the age and the chevron on one line,
  // then the title and the detail across the full width, so a title never wraps word by word
  // in a column squeezed by the trailing items.
  const body = (pressed: boolean) =>
    design.accessibilityLayout ? (
      <View testID={`notification-${row.id}-body`} style={[styles.stacked, pressedStyle(pressed)]}>
        <View style={styles.stackedTop}>
          {leading}
          <View style={styles.flex} />
          {age}
          {chevron}
        </View>
        {title}
        {detail}
      </View>
    ) : (
      <View testID={`notification-${row.id}-body`} style={[styles.row, pressedStyle(pressed)]}>
        {leading}
        <View style={styles.text}>
          {title}
          {detail}
        </View>
        {age}
        {chevron}
      </View>
    );
  if (ref === null) {
    return (
      <View {...a11y} accessible accessibilityLabel={label} style={{ backgroundColor: colors.surface }}>
        {body(false)}
      </View>
    );
  }
  return (
    <Pressable
      {...a11y}
      accessibilityRole="link"
      accessibilityLabel={label}
      accessibilityHint="Opens the conversation"
      android_ripple={ripple(design)}
      onPress={() => onOpen(row, ref)}
      style={{ backgroundColor: colors.surface }}>
      {({ pressed }) => body(pressed && uiPlatform === 'ios')}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  grow: { flexGrow: 1 },
  list: { paddingBottom: 24 },
  loading: { padding: 32 },
  chipBar: { flexGrow: 0, flexShrink: 0 },
  chips: { alignItems: 'center', gap: 8, paddingTop: 4, paddingBottom: 12 },
  section: { marginBottom: 20 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingStart: 8, paddingEnd: 16, paddingVertical: 10 },
  leading: { flexDirection: 'row', alignItems: 'center', gap: 6 },
  dot: { width: 8, height: 8, borderRadius: 4 },
  text: { flex: 1, gap: 2 },
  flex: { flex: 1 },
  stacked: { gap: 6, paddingStart: 8, paddingEnd: 16, paddingVertical: 12 },
  stackedTop: { flexDirection: 'row', alignItems: 'center', gap: 12 },
});
